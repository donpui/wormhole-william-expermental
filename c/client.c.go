//+build cgo

package main

import (
	"context"
	"fmt"
	"unsafe"
	"log"

	"github.com/psanford/wormhole-william/c/codes"
	"github.com/psanford/wormhole-william/wormhole"
	"io/ioutil"
)

import "C"

func main() {

}

// TODO: refactor?
const (
	DEFAULT_APP_ID                      = "myFileTransfer"
	DEFAULT_RENDEZVOUS_URL              = "ws://relay.magic-wormhole.io:4000/v1"
	DEFAULT_TRANSIT_RELAY_URL           = "tcp:transit.magic-wormhole.io:4001"
	DEFAULT_PASSPHRASE_COMPONENT_LENGTH = 2
)

// TODO: figure out how to get uintptr key to work.
type ClientsMap = map[uintptr]*wormhole.Client

var (
	ErrClientNotFound = fmt.Errorf("%s", "wormhole client not found")

	clientsMap ClientsMap
)

func init() {
	clientsMap = make(ClientsMap)
}

//export NewClient
func NewClient() uintptr {
	// TODO: receive config
	client := &wormhole.Client{
		AppID: DEFAULT_APP_ID,
		RendezvousURL: DEFAULT_RENDEZVOUS_URL,
		TransitRelayURL: DEFAULT_TRANSIT_RELAY_URL,
		PassPhraseComponentLength: DEFAULT_PASSPHRASE_COMPONENT_LENGTH,
	}

	clientPtr := uintptr(unsafe.Pointer(client))
	clientsMap[clientPtr] = client

	return clientPtr
}

//export FreeClient
func FreeClient(clientPtr uintptr) int {
	if _, err := getClient(clientPtr); err != nil {
		return int(codes.ERR_NO_CLIENT)
	}

	delete(clientsMap, clientPtr)
	return int(codes.OK)
}

//export ClientSendText
func ClientSendText(clientPtr uintptr, msgC *C.char, codeOutC **C.char) int {
	client, err := getClient(clientPtr)
	if err != nil {
		return int(codes.ERR_NO_CLIENT)
	}
	ctx := context.Background()

	code, status, err := client.SendText(ctx, C.GoString(msgC))
	if err != nil {
		log.Printf("%v\n", err)
		return int(codes.ERR_SEND_TEXT)
	}

	*codeOutC = C.CString(code)
	return int(codes.OK)
}

//export ClientRecvText
func ClientRecvText(clientPtr uintptr, codeC *C.char, msgOutC **C.char) int {
	client, err := getClient(clientPtr)
	if err != nil {
		return int(codes.ERR_NO_CLIENT)
	}
	ctx := context.Background()

	msg, err := client.Receive(ctx, C.GoString(codeC))
	if err != nil {
		return int(codes.ERR_SEND_TEXT)
	}

	data, err := ioutil.ReadAll(msg)
	if err != nil {
		return int(codes.ERR_RECV_TEXT)
	}

	*msgOutC = C.CString(string(data))
	return int(codes.OK)
}

// TODO: refactor w/ wasm package?
func getClient(clientPtr uintptr) (*wormhole.Client, error) {
	client, ok := clientsMap[clientPtr]
	if !ok {
		fmt.Printf("clientMap entry missing: %d\n", uintptr(clientPtr))
		fmt.Printf("clientMap entry missing: %d\n", clientPtr)
		return nil, ErrClientNotFound
	}

	return client, nil
}
