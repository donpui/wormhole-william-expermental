package wormhole

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/psanford/wormhole-william/internal/crypto"
)

type dilationProtocol struct {
	versions         []string
	state            DilationState
	stateMu          sync.Mutex
	managerState     ManagerState
	managerStateMu   sync.Mutex
	managerInputEv   chan ManagerInputEvent
	connectorState   ConnectorState
	connectorStateMu sync.Mutex
	l2ConnState      L2ConnState
	l2ConnStateMu    sync.Mutex
	msgInput         chan []byte
	role             Role
	side             string
	// The code mostly sans-io approach: functional core,
	// imperative shell.
	//
	// XXX: The type should have a channel to receive input events
	// and messages from I/O shell and a channel to send output
	// events and messages. These output events can be thought of
	// as commands on what the I/O layer needs to do. The timeouts
	// etc are handled in the I/O layer and conveyed to the core
	// via events. Core would produce output events or commands in
	// response. Some of these events/commands may also carry a
	// payload that the command needs to act on. In short, it is a
	// state machine that produces new state and an output in
	// response to inputs.
}

type DilationState int
type Role string

type ManagerState string
type ManagerInputEvent int
type ManagerOutputEvent int

type ConnectorState string
type ConnectorInputEvent int
type ConnectorOutputEvent int

type L2ConnState string
type L2ConnInputEvent int
type L2ConnOutputEvent int

const (
	DilationNotNegotiated DilationState = -1
	DilationImpossible    DilationState = iota
	DilationPossible
)

const (
	ManagerStateWaiting    ManagerState = "ManagerStateWaiting"
	ManagerStateWanting    ManagerState = "ManagerStateWanting"
	ManagerStateConnecting ManagerState = "ManagerStateConnecting"
	ManagerStateConnected  ManagerState = "ManagerStateConnected"
	ManagerStateFlushing   ManagerState = "ManagerStateFlushing"
	ManagerStateLonely     ManagerState = "ManagerStateLonely"
	ManagerStateAbandoning ManagerState = "ManagerStateAbandoning"
	ManagerStateStopping   ManagerState = "ManagerStateStopping"
	ManagerStateStopped    ManagerState = "ManagerStateStopped"
)

const (
	ManagerInputEventStart = iota
	ManagerInputEventRxPlease
	ManagerInputEventConnectionMade
	ManagerInputEventRxReconnecting
	ManagerInputEventRxReconnect
	ManagerInputEventConnectionLostLeader
	ManagerInputEventConnectionLostFollower
	ManagerInputEventRxHints
	ManagerInputEventStop
)

const (
	ManagerOutputEventSendPlease = iota
	ManagerOutputEventNotifyStopped
	ManagerOutputEventRxHints
	ManagerOutputEventChooseRole
	ManagerOutputEventStartConnectingIgnoreMsg
	ManagerOutputEventUseHints
	ManagerOutputEventStopConnecting
	ManagerOutputEventSendReconnecting
	ManagerOutputEventStartConnecting
	ManagerOutputEventSendReconnect
	ManagerOutputEventAbandonConnection
)

const (
	ConnectorStateConnecting ConnectorState = "ConnectorStateConnecting"
	ConnectorStateConnected  ConnectorState = "ConnectorStateConnected"
	ConnectorStateStopped    ConnectorState = "ConnectorStateStopped"
)

const (
	ConnectorInputEventListenerReady = iota
	ConnectorInputEventAccept
	ConnectorInputEventAddCandidate
	ConnectorInputEventGotHints
	ConnectorInputEventAddRelay
	ConnectorInputEventStop
)

// XXX placeholder for a set of connections
// XXX replace with the proper type later
type Candidate string

type ConnectorInputEventS struct {
	Event      ConnectorInputEvent // 0 - ListenerReady, 1 - Accept, 2 - AddCandidate, 4 - GotHints, 5 - AddRelay, 6 - Stop 
	hints   transitHintsV1
	candidate Candidate
}

const (
	ConnectorOutputEventPublishHints = iota
	ConnectorOutputEventSelectAndStopRemaining
	ConnectorOutputEventConsider
	ConnectorOutputEventUseHints
	ConnectorOutputEventStopEverything
)

type ConnectorOutputEventS struct {
	Event ConnectorOutputEvent
	hints  transitHintsV1
	candidate Candidate
}

const (
	L2ConnStateUnselected L2ConnState = "L2ConnStateUnselected"
	L2ConnStateSelecting = "L2ConnStateSelecting"
	L2ConnStateSelected = "L2ConnStateSelected"
)

const (
	L2ConnInputEventGotKCM = iota
	L2ConnInputEventSelect
	L2ConnInputEventGotRecord
)

type L2ConnInputEventS struct {
	Event L2ConnInputEvent
	candidate Candidate
}

const (
	L2ConnOutputEventAddCandidate = iota
	L2ConnOutputEventSetManager
	L2ConnOutputEventCanSendRecords
	L2ConnOutputEventProcessInboundQueue
	L2ConnOutputEventQueueInboundRecord
	L2ConnOutputEventDeliverRecord
)

type L2ConnOutputEventS struct {
	Event L2ConnOutputEvent
	candidate Candidate
}

const (
	Leader   Role = "Leader"
	Follower Role = "Follower"
)

type pleaseMsg struct {
	// because type is a reserved keyword
	tipe string `json:"type"`
	side string `json:"side"`
	// XXX: docs rightly talks about a "use-version" field that
	// would calculate the version to use based on the earlier
	// "can-dilate" field in the versions message. But in the
	// python code, it seem to have been omitted. Perhaps it is a
	// good thing to re-instate it in the code.
}

type hints struct {
	tipe     string  `json:"type"`
	priority float32 `json:"priority"`
	hostname string  `json:"hostname"`
	port     int     `json:"port"`
}

type connectionHintsMsg struct {
	tipe  string  `json:"type"`
	hints []hints `json:"connection-hints"`
}

type dilateAddMsg struct {
	tipe  string `json:"type"`  // "type": "add"
	phase string `json:"phase"` // "phase": "dilate-${n}"
	body  string `json:"body"`  // "body": <encrypted contents>
	// XXX: id? "id": "2-byte hex string"
}

func genSide() string {
	return crypto.RandSideID()
}

func InitDilation() *dilationProtocol {
	mySide := genSide()
	return &dilationProtocol{
		versions:     []string{"1"},
		side:         mySide,
		managerState: ManagerStateWaiting,
	}
}

func (d *dilationProtocol) chooseRole(otherSide string) error {
	if d.side > otherSide {
		d.role = Leader
	} else if d.side < otherSide {
		d.role = Follower
	} else {
		return errors.New("sides shouldn't be equal")
	}
	return nil
}

// like sending a message via mailbox, but instead of numbered phases,
// it will use phase names like "dilate-1", "dilate-2" .. etc
func genDilationMsg(payload []byte, phase int) ([]byte, error) {
	// everytime, we use the existing "phase" to compute the
	// "dilation-$n" field. Maintaining the phase needs to happen
	// outside the core as it is state and so has side effects.

	var msg dilateAddMsg
	msg.tipe = "add"
	msg.phase = fmt.Sprintf("dilate-%d", phase)
	msg.body = hex.EncodeToString(payload)

	return json.Marshal(msg)
}

func (d *dilationProtocol) sendDilationMsg() {

}

// once dilation capability is confirmed for both the sides,
// send the "please" message. "please" is how we learn about the
// other side's "side".
func (d *dilationProtocol) genPleaseMsg() ([]byte, error) {
	please := pleaseMsg{
		tipe: "please", // how wasteful! Can't we autogenerate this?
		side: d.side,
	}
	return json.Marshal(please)
}

// State -> a -> (State, b) state machines take the current state, an
// input of type a and gives a new state and a new value of type b.
// In our case, the state is in the type dilationProtocol. Input is a
// bytestring. New state would mutate the current state in d. Output
// would be a bunch of functions that operate on input.
//
// XXX: We would run this state machine in a go routine? The input
// could come via a channel then?
//
// XXX: how do we represent the output?
func (d *dilationProtocol) managerToNewState(newState ManagerState) {
	d.managerStateMu.Lock()
	defer d.managerStateMu.Unlock()

	d.managerState = newState
}

func (d *dilationProtocol) connectorToNewState(newState ConnectorState) {
	d.connectorStateMu.Lock()
	defer d.connectorStateMu.Unlock()

	d.connectorState = newState
}

func (d *dilationProtocol) l2ConnToNewState(newState L2ConnState) {
	d.l2ConnStateMu.Lock()
	defer d.l2ConnStateMu.Unlock()

	d.l2ConnState = newState
}

func (d *dilationProtocol) getState() ManagerState {
	d.managerStateMu.Lock()
	defer d.managerStateMu.Unlock()

	return d.managerState
}

// warning: This is a giant nested switch-case statement and is hard
// to read. This function would process one input event at a
// particular state and move to state (if needed) to another state and
// produce output events. The caller also has access to the input payload
// that the functions tied to output events may need to work on.

// step: input_event, current_state -> (output_State, [output_events])

func (d *dilationProtocol) managerStateMachine(event ManagerInputEvent) []ManagerOutputEvent {
	// event := <-d.managerInputEv
	var currState ManagerState
	var nextState ManagerState
	var outputEvents []ManagerOutputEvent

	currState = d.managerState
	nextState = d.managerState

	switch event {
	case ManagerInputEventStart:
		// if current state is WAITING, then go to WANTING and
		// send "please" to peer.
		switch currState {
		case ManagerStateWaiting:
			nextState = ManagerStateWanting
			d.managerToNewState(nextState)
			// XXX: send please message
			outputEvents = []ManagerOutputEvent{ManagerOutputEventSendPlease}
		default:
			// other states should ignore the start event
		}
	case ManagerInputEventRxPlease:
		switch currState {
		case ManagerStateWanting:
			// upon receiving rx_please at WANTING, move
			// to CONNECTING and output choose_role,
			// start_connecting_ignore_message
			nextState = ManagerStateConnecting
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{
				ManagerOutputEventChooseRole,
				ManagerOutputEventStartConnectingIgnoreMsg,
			}
		default:
		}
	case ManagerInputEventConnectionMade:
		// generated by the connector
		switch currState {
		case ManagerStateConnecting:
			nextState = ManagerStateConnected
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{}
		default:
		}
	case ManagerInputEventRxReconnecting:
		switch currState {
		case ManagerStateFlushing:
			nextState = ManagerStateConnecting
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventStartConnecting}
		default:
		}
	case ManagerInputEventRxReconnect:
		switch currState {
		case ManagerStateConnected:
			nextState = ManagerStateAbandoning
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventAbandonConnection}
		case ManagerStateConnecting:
			outputEvents = []ManagerOutputEvent{
				ManagerOutputEventStopConnecting,
				ManagerOutputEventSendReconnecting,
				ManagerOutputEventStartConnecting,
			}
		case ManagerStateLonely:
			nextState = ManagerStateConnecting
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{
				ManagerOutputEventSendReconnecting,
				ManagerOutputEventStartConnecting,
			}
		default:
		}
	case ManagerInputEventConnectionLostLeader:
		switch currState {
		case ManagerStateConnected:
			nextState = ManagerStateFlushing
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventSendReconnect}
		case ManagerStateStopping:
			nextState = ManagerStateStopped
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventNotifyStopped}
		default:
		}
	case ManagerInputEventConnectionLostFollower:
		switch currState {
		case ManagerStateConnected:
			nextState = ManagerStateLonely
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{}
		case ManagerStateAbandoning:
			nextState = ManagerStateConnecting
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{
				ManagerOutputEventSendReconnecting,
				ManagerOutputEventStartConnecting,
			}
		case ManagerStateStopping:
			nextState = ManagerStateStopped
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventNotifyStopped}
		default:
		}
	case ManagerInputEventRxHints:
		switch currState {
		case ManagerStateWanting, ManagerStateConnected, ManagerStateAbandoning, ManagerStateFlushing, ManagerStateLonely, ManagerStateStopping:
			// do nothing, stay in WANTING.
			//
			// XXX we can as well omit this case statement
			// but leaving it here for now.
			outputEvents = []ManagerOutputEvent{}
		case ManagerStateConnecting:
			outputEvents = []ManagerOutputEvent{ManagerOutputEventUseHints}
		default: // other states ignore rx_hints
		}
	case ManagerInputEventStop:
		switch currState {
		case ManagerStateWaiting, ManagerStateWanting, ManagerStateLonely, ManagerStateFlushing:
			nextState = ManagerStateStopped
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventNotifyStopped}
		case ManagerStateConnecting:
			nextState = ManagerStateStopped
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventStopConnecting, ManagerOutputEventNotifyStopped}
		case ManagerStateConnected:
			nextState = ManagerStateStopping
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{ManagerOutputEventAbandonConnection}
		case ManagerStateAbandoning:
			nextState = ManagerStateStopping
			d.managerToNewState(nextState)
			outputEvents = []ManagerOutputEvent{}
		default:
		}
	default:
		log.Printf("dilation manager fsm: unknown input event - %d\n", event)
	}

	log.Printf("Manager FSM transition: %s -> %s\n", currState, nextState)

	return outputEvents
}

// receives decrypted dilate-$n payloads (but still in json)
func (d *dilationProtocol) receiveDilationMsg() {
	eventMap := map[string]ManagerInputEvent{
		"please":           ManagerInputEventRxPlease,
		"connection-hints": ManagerInputEventRxHints,
		"reconnect":        ManagerInputEventRxReconnect,
		"reconnecting":     ManagerInputEventRxReconnecting,
	}

	// this go routine sits here waiting for incoming network
	// bytestream and convert to a manager event and push the
	// event into manager's input event queue (a channel)
	go func() {
		var result map[string]interface{}
		for plaintext := range d.msgInput {
			err := json.Unmarshal(plaintext, &result)
			if err != nil {
				// XXX send an error msg via a channel
			}
			eventTxt, ok := result["type"]
			if ok {
				// convert event msg into a value of
				// input event type (which is an int
				// underneath)
				event, ok := eventMap[eventTxt.(string)]
				if ok {
					// push the input event into manager input event channel
					d.managerInputEv <- event
				} else {
					// XXX log the lookup error
				}
			} else {
				// XXX log the lookup error
			}
		}
	}()
}

func (d *dilationProtocol) connectorStateMachine(event ConnectorInputEvent) []ConnectorOutputEvent {
	var currState ConnectorState
	var nextState ConnectorState
	var outputEvents []ConnectorOutputEvent

	currState = d.connectorState
	nextState = d.connectorState

	switch event {
	case ConnectorInputEventListenerReady:
		switch currState {
		case ConnectorStateConnecting:
			outputEvents = []ConnectorOutputEvent{ConnectorOutputEventPublishHints}
		case ConnectorStateConnected:
		default:
		}
	case ConnectorInputEventAccept:
		switch currState {
		case ConnectorStateConnecting:
			nextState = ConnectorStateConnected
			d.connectorToNewState(nextState)
			outputEvents = []ConnectorOutputEvent{ConnectorOutputEventSelectAndStopRemaining}
		default:
		}
	case ConnectorInputEventAddCandidate:
		switch currState {
		case ConnectorStateConnecting:
			outputEvents = []ConnectorOutputEvent{ConnectorOutputEventConsider}
		default:
		}
	case ConnectorInputEventGotHints:
		switch currState {
		case ConnectorStateConnecting:
			outputEvents = []ConnectorOutputEvent{ConnectorOutputEventUseHints}
		default:
		}
	case ConnectorInputEventAddRelay:
		switch currState {
		case ConnectorStateConnecting:
			outputEvents = []ConnectorOutputEvent{
				ConnectorOutputEventUseHints,
				ConnectorOutputEventPublishHints,
			}
		default:
		}
	case ConnectorInputEventStop:
		switch currState {
		case ConnectorStateConnecting, ConnectorStateConnected:
			nextState = ConnectorStateStopped
			d.connectorToNewState(nextState)
			outputEvents = []ConnectorOutputEvent{
				ConnectorOutputEventStopEverything,
			}
		}
	}
	log.Printf("Connector FSM transition: %s -> %s\n", currState, nextState)
	return outputEvents
}

// each input and output event carry some kind of a payload. So, we need to somehow move that payload from an input event to an output event
func (d *dilationProtocol) processConnectorStateMachine(input ConnectorInputEventS) []ConnectorOutputEventS {
	outputs := d.connectorStateMachine(input.Event)
	outputEvents := []ConnectorOutputEventS{}

	for output := range outputs {
		switch output {
		case ConnectorOutputEventPublishHints:
			outputEvents = append(outputEvents, ConnectorOutputEventS{
				Event: ConnectorOutputEventPublishHints,
				hints: input.hints,
			})
		case ConnectorOutputEventSelectAndStopRemaining:
			outputEvents = append(outputEvents, ConnectorOutputEventS{
				Event: ConnectorOutputEventSelectAndStopRemaining,
				candidate: input.candidate,
			})
		case ConnectorOutputEventConsider:
			outputEvents = append(outputEvents, ConnectorOutputEventS{
				Event: ConnectorOutputEventConsider,
				candidate: input.candidate,
			})
		case ConnectorOutputEventUseHints:
			outputEvents = append(outputEvents, ConnectorOutputEventS{
				Event: ConnectorOutputEventUseHints,
				hints: input.hints,
			})
		case ConnectorOutputEventStopEverything:
			outputEvents = append(outputEvents, ConnectorOutputEventS{
				Event: ConnectorOutputEventStopEverything,
			})
		default:
		}
	}

	return outputEvents
}

// L2 connection state machine. At any point, there is only one active
// L2 connection. Leader and Follower initiate many simultaneous
// connections of which some of them would connect and handshake. One
// of those would be selected by the leader.
func (d *dilationProtocol) l2ConnStateMachine(event L2ConnInputEvent) []L2ConnOutputEvent {
	var currState L2ConnState
	var nextState L2ConnState
	var outputEvents []L2ConnOutputEvent

	currState = d.l2ConnState
	nextState = d.l2ConnState

	switch event {
	case L2ConnInputEventGotKCM:
		switch currState {
		case L2ConnStateUnselected:
			nextState = L2ConnStateSelecting
			d.l2ConnToNewState(nextState)
			outputEvents = []L2ConnOutputEvent{
				L2ConnOutputEventAddCandidate,
			}
		default:
		}
	case L2ConnInputEventSelect:
		switch currState {
		case L2ConnStateSelecting:
			nextState = L2ConnStateSelected
			d.l2ConnToNewState(nextState)
			outputEvents = []L2ConnOutputEvent{
				L2ConnOutputEventSetManager,
				L2ConnOutputEventCanSendRecords,
				L2ConnOutputEventProcessInboundQueue,
			}
		}
	case L2ConnInputEventGotRecord:
		switch currState {
		case L2ConnStateSelecting:
			outputEvents = []L2ConnOutputEvent{
				L2ConnOutputEventQueueInboundRecord,
			}
		case L2ConnStateSelected:
			outputEvents = []L2ConnOutputEvent{
				L2ConnOutputEventDeliverRecord,
			}
		default:
		}
	default:
	}
	log.Printf("L2 Connection FSM transition: %s -> %s\n", currState, nextState)
	return outputEvents
}

func (d *dilationProtocol) processL2ConnStateMachine(input L2ConnInputEventS) []L2ConnOutputEventS {
	outputs := d.l2ConnStateMachine(input.Event)
	outputEvents := []L2ConnOutputEventS{}

	for output := range outputs {
		switch output {
		case L2ConnOutputEventAddCandidate:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventAddCandidate,
				candidate: input.candidate,
			})
		case L2ConnOutputEventSetManager:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventSetManager,
				// XXX there is ready access to manager, do this may not be useful here.
			})
		case L2ConnOutputEventCanSendRecords:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventCanSendRecords,
				// XXX: this would set a boolean elsewhere?
			})
		case L2ConnOutputEventProcessInboundQueue:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventProcessInboundQueue,
				// XXX: this is for the outer layers to act on
			})
		case L2ConnOutputEventQueueInboundRecord:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventQueueInboundRecord,
				// XXX: we shouldn't be copying records here, it is for the I/O later to act on this message.
			})
		case L2ConnOutputEventDeliverRecord:
			outputEvents = append(outputEvents, L2ConnOutputEventS{
				Event: L2ConnOutputEventDeliverRecord,
				// XXX: outer later should act on this and send appropriate message along with payload to manager FSM.
			})
		default:
		}
	}

	return outputEvents
}
