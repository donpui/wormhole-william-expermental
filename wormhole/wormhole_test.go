package wormhole

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zip"
	"github.com/psanford/wormhole-william/internal/crypto"
	"github.com/psanford/wormhole-william/rendezvous"
	"github.com/psanford/wormhole-william/rendezvous/rendezvousservertest"
	"nhooyr.io/websocket"
)

var relayServerConstructors = map[string]func() *testRelayServer{
	"TCP": newTestTCPRelayServer,
	"WS":  newTestWSRelayServer,
}

func TestWormholeSendRecvText(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	// disable transit relay
	DefaultTransitRelayURL = "tcp://"

	var c0Verifier string
	var c0 Client
	c0.RendezvousURL = url
	c0.VerifierOk = func(code string) bool {
		c0Verifier = code
		return true
	}

	var c1Verifier string
	var c1 Client
	c1.RendezvousURL = url
	c1.VerifierOk = func(code string) bool {
		c1Verifier = code
		return true
	}

	secretText := "Hialeah-deviltry"
	code, statusChan, err := c0.SendText(ctx, secretText)
	if err != nil {
		t.Fatal(err)
	}

	nameplate := strings.SplitN(code, "-", 2)[0]

	// recv with wrong code
	_, err = c1.Receive(ctx, fmt.Sprintf("%s-intermarrying-aliased", nameplate), false)
	if err != errDecryptFailed {
		t.Fatalf("Recv side expected decrypt failed due to wrong code but got: %s", err)
	}

	status := <-statusChan
	if status.OK || status.Error != errDecryptFailed {
		t.Fatalf("Send side expected decrypt failed but got status: %+v", status)
	}

	code, statusChan, err = c0.SendText(ctx, secretText)
	if err != nil {
		t.Fatal(err)
	}

	// recv with correct code
	msg, err := c1.Receive(ctx, code, false)
	if err != nil {
		t.Fatalf("Recv side got unexpected err: %s", err)
	}

	msgBody, err := ioutil.ReadAll(msg)
	if err != nil {
		t.Fatalf("Recv side got read err: %s", err)
	}

	if string(msgBody) != secretText {
		t.Fatalf("Got Message does not match sent secret got=%s sent=%s", msgBody, secretText)
	}

	status = <-statusChan
	if !status.OK || status.Error != nil {
		t.Fatalf("Send side expected OK status but got: %+v", status)
	}

	if c0Verifier != c1Verifier {
		t.Fatalf("Expected verifiers to match but were different")
	}

	// Send with progress
	// we should get one update for progress when we get the ok
	// result back from the receiver
	secretText = "retrospectives-𐄷-cropper"
	var (
		progressSentBytes  int64
		progressTotalBytes int64
		progressCallCount  int
	)
	progressFunc := func(sentBytes int64, totalBytes int64) {
		progressCallCount++
		progressSentBytes = sentBytes
		progressTotalBytes = totalBytes
	}
	code, statusChan, err = c0.SendText(ctx, secretText, WithProgress(progressFunc))
	if err != nil {
		t.Fatal(err)
	}

	// recv with correct code
	msg, err = c1.Receive(ctx, code, false)
	if err != nil {
		t.Fatalf("Recv side got unexpected err: %s", err)
	}

	msgBody, err = ioutil.ReadAll(msg)
	if err != nil {
		t.Fatalf("Recv side got read err: %s", err)
	}

	if string(msgBody) != secretText {
		t.Fatalf("Got Message does not match sent secret got=%s sent=%s", msgBody, secretText)
	}

	status = <-statusChan
	if !status.OK || status.Error != nil {
		t.Fatalf("Send side expected OK status but got: %+v", status)
	}

	if c0Verifier != c1Verifier {
		t.Fatalf("Expected verifiers to match but were different")
	}

	if progressCallCount != 1 {
		t.Fatalf("progressCallCount got %d expected 1", progressCallCount)
	}

	if progressSentBytes != int64(len(msgBody)) {
		t.Fatalf("progressSentBytes got %d expected %d", progressSentBytes, int64(len(msgBody)))
	}

	if progressTotalBytes != int64(len(msgBody)) {
		t.Fatalf("progressTotalBytes got %d expected %d", progressTotalBytes, int64(len(msgBody)))
	}
}

func TestVerifierAbort(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	// disable transit relay
	DefaultTransitRelayURL = "tcp://"

	var c0 Client
	c0.RendezvousURL = url
	c0.VerifierOk = func(code string) bool {
		return false
	}

	var c1 Client
	c1.RendezvousURL = url
	c1.VerifierOk = func(code string) bool {
		return true
	}

	secretText := "minced-incalculably"
	code, statusChan, err := c0.SendText(ctx, secretText)
	if err != nil {
		t.Fatal(err)
	}

	// recv with correct code
	_, err = c1.Receive(ctx, code, false)
	expectErr := errors.New("TransferError: sender rejected verification check, abandoned transfer")
	if err.Error() != expectErr.Error() {
		t.Fatalf("Expected recv err %q got %q", expectErr, err)
	}

	status := <-statusChan
	expectErr = errors.New("sender rejected verification check, abandoned transfer")
	if status.Error.Error() != expectErr.Error() {
		t.Fatalf("Send side expected %q error but got: %q", expectErr, status.Error)
	}
}

func TestWormholeFileReject(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	// disable transit relay for this test
	DefaultTransitRelayURL = "tcp://"

	var c0 Client
	c0.RendezvousURL = url

	var c1 Client
	c1.RendezvousURL = url

	fileContent := make([]byte, 1<<16)
	for i := 0; i < len(fileContent); i++ {
		fileContent[i] = byte(i)
	}

	buf := bytes.NewReader(fileContent)

	code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, false)
	if err != nil {
		t.Fatal(err)
	}

	receiver, err := c1.Receive(ctx, code, false)
	if err != nil {
		t.Fatal(err)
	}

	receiver.Reject()

	result := <-resultCh
	expectErr := "TransferError: transfer rejected"
	expectMood := "happy"

	//checks clients closure status in Mailbox
	closeMoods := rs.CloseMoods()

	if closeMoods == nil {
		t.Fatalf("Expected to get mood, but got nothing")
	}

	for side, mood := range closeMoods {
		if mood != expectMood {
			t.Fatalf("Expected %q result, but got: %+v, side: %+v", expectMood, mood, side)
		}
	}
	if result.Error.Error() != expectErr {
		t.Fatalf("Expected %q result, but got: %+v", expectErr, result)
	}
}

func TestWormholeFileTransportSendRecvViaRelayServer(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			relayURL := relayServer.url.String()
			defer relayServer.close()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURL

			fileContent := make([]byte, 1<<16)
			for i := 0; i < len(fileContent); i++ {
				fileContent[i] = byte(i)
			}

			buf := bytes.NewReader(fileContent)

			code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, fileContent) {
				t.Fatalf("File contents mismatch")
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}
		})
	}
}

func TestWormholeBigFileTransportSendRecvViaRelayServer(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			relayURL := relayServer.url.String()
			defer relayServer.close()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURL

			// Create a fake file offer
			var fakeBigSize int64 = 32098461509
			offer := &offerMsg{
				File: &offerFile{
					FileName: "fakefile",
					FileSize: fakeBigSize,
				},
			}

			// just a pretend reader
			r := bytes.NewReader(make([]byte, 1))

			// skip th wrapper so we can provide our own offer
			code, _, err := c0.sendFileDirectory(ctx, offer, r, true)
			//c0.SendFile(ctx, "file.txt", buf)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			if int64(receiver.TransferBytes64) != fakeBigSize {
				t.Fatalf("Mismatch in size between what we are trying to send and what is (our parsed) offer. Expected %v but got %v", fakeBigSize, receiver.TransferBytes64)
			}
		})
	}
}

func TestWormholeFileTransportRecvMidStreamCancel(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			relayURL := relayServer.url.String()
			defer relayServer.close()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURL

			fileContent := make([]byte, 1<<16)
			for i := 0; i < len(fileContent); i++ {
				fileContent[i] = byte(i)
			}

			buf := bytes.NewReader(fileContent)

			code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, true)
			if err != nil {
				t.Fatal(err)
			}

			childCtx, cancel := context.WithCancel(ctx)

			receiver, err := c1.Receive(childCtx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			initialBuffer := make([]byte, 1<<10)

			_, err = io.ReadFull(receiver, initialBuffer)
			if err != nil {
				t.Fatal(err)
			}

			cancel()

			_, err = ioutil.ReadAll(receiver)
			if err == nil {
				t.Fatalf("Expected read error but got none")
			}

			result := <-resultCh
			fmt.Println(result)
			if result.OK {
				t.Fatalf("Expected error result but got ok")
			}
		})
	}
}

func TestWormholeFileTransportSendMidStreamCancel(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {

			relayServer := newRelayServer()
			relayURL := relayServer.url.String()
			defer relayServer.close()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURL

			fileContent := make([]byte, 1<<16)
			for i := 0; i < len(fileContent); i++ {
				fileContent[i] = byte(i)
			}

			sendCtx, cancel := context.WithCancel(ctx)

			splitR := splitReader{
				Reader:   bytes.NewReader(fileContent),
				cancelAt: 1 << 10,
				cancel:   cancel,
			}

			code, resultCh, err := c0.SendFile(sendCtx, "file.txt", &splitR, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			_, err = ioutil.ReadAll(receiver)
			if err == nil {
				t.Fatal("Expected read error but got none")
			}

			result := <-resultCh
			if result.OK {
				t.Fatal("Expected send resultCh to error but got none")
			}
		})
	}
}

func TestPendingSendCancelable(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			defer relayServer.close()

			c0 := Client{
				RendezvousURL:   url,
				TransitRelayURL: relayServer.url.String(),
			}

			fileContent := make([]byte, 1<<16)
			for i := 0; i < len(fileContent); i++ {
				fileContent[i] = byte(i)
			}

			buf := bytes.NewReader(fileContent)

			childCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			code, resultCh, err := c0.SendFile(childCtx, "file.txt", buf, false)
			if err != nil {
				t.Fatal(err)
			}

			// connect to mailbox to wait for c0 to write its initial message
			rc := rendezvous.NewClient(url, crypto.RandSideID(), c0.AppID)

			_, err = rc.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}

			defer rc.Close(ctx, rendezvous.Happy)
			nameplate, err := nameplateFromCode(code)
			if err != nil {
				t.Fatal(err)
			}

			err = rc.AttachMailbox(ctx, nameplate)
			if err != nil {
				t.Fatal(err)
			}

			msgs := rc.MsgChan(ctx)

			select {
			case <-msgs:
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for c0 to send a message")
			}

			cancel()

			select {
			case result := <-resultCh:
				if result.OK {
					t.Fatalf("Expected cancellation error but got OK")
				}
				if result.Error == nil {
					t.Fatalf("Expected cancellation error")
				}
			case <-time.After(5 * time.Second):
				// log all goroutines
				pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
				t.Fatalf("Wait for result timed out")
			}
		})
	}
}

func TestPendingRecvCancelable(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			defer relayServer.close()

			c0 := Client{
				RendezvousURL:   url,
				TransitRelayURL: relayServer.url.String(),
			}

			childCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			code := "87-firetrap-fallacy"

			// wait to see mailbox has been allocated, and then
			// wait to see PAKE message from receiver
			rc := rendezvous.NewClient(url, crypto.RandSideID(), c0.AppID)

			_, err := rc.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}

			defer rc.Close(ctx, rendezvous.Happy)

			for i := 0; i < 20; i++ {
				nameplates, err := rc.ListNameplates(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(nameplates) > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}

			defer rc.Close(ctx, rendezvous.Happy)
			nameplate, err := nameplateFromCode(code)
			if err != nil {
				t.Fatal(err)
			}

			err = rc.AttachMailbox(ctx, nameplate)
			if err != nil {
				t.Fatal(err)
			}

			resultCh := make(chan error, 1)
			go func() {
				_, err := c0.Receive(childCtx, code, false)
				resultCh <- err
			}()

			msgs := rc.MsgChan(ctx)

			select {
			case <-msgs:
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for c0 to send a message")
			}

			cancel()

			select {
			case gotErr := <-resultCh:
				if gotErr == nil {
					t.Fatalf("Expected an error but got none")
				}
			case <-time.After(5 * time.Second):
				// log all goroutines
				pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
				t.Fatalf("Timeout waiting for recv cancel")
			}
		})
	}
}

func TestWormholeDirectoryTransportSendRecvDirect(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	// disable transit relay for this test
	DefaultTransitRelayURL = "tcp://"

	var c0Verifier string
	var c0 Client
	c0.RendezvousURL = url
	c0.VerifierOk = func(code string) bool {
		c0Verifier = code
		return true
	}

	var c1Verifier string
	var c1 Client
	c1.RendezvousURL = url
	c1.VerifierOk = func(code string) bool {
		c1Verifier = code
		return true
	}

	personalizeContent := make([]byte, 1<<16)
	for i := 0; i < len(personalizeContent); i++ {
		personalizeContent[i] = byte(i)
	}

	bodiceContent := []byte("placarding-whereat")

	entries := []DirectoryEntry{
		{
			Path: filepath.Join("skyjacking", "personalize.txt"),
			Reader: func() (io.ReadCloser, error) {
				b := bytes.NewReader(personalizeContent)
				return ioutil.NopCloser(b), nil
			},
		},
		{
			Path: filepath.Join("skyjacking", "bodice-Maytag.txt"),
			Reader: func() (io.ReadCloser, error) {
				b := bytes.NewReader(bodiceContent)
				return ioutil.NopCloser(b), nil
			},
		},
	}

	code, resultCh, err := c0.SendDirectory(ctx, "skyjacking", entries, false)
	if err != nil {
		t.Fatal(err)
	}

	receiver, err := c1.Receive(ctx, code, false)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ioutil.ReadAll(receiver)
	if err != nil {
		t.Fatal(err)
	}

	r, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := ioutil.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()

		if f.Name == "personalize.txt" {
			if !bytes.Equal(body, personalizeContent) {
				t.Fatal("personalize.txt file content does not match")
			}
		} else if f.Name == "bodice-Maytag.txt" {
			if !bytes.Equal(bodiceContent, body) {
				t.Fatalf("bodice-Maytag.txt file content does not match %s vs %s", bodiceContent, body)
			}
		} else {
			t.Fatalf("Unexpected file %s", f.Name)
		}
	}

	result := <-resultCh
	if !result.OK {
		t.Fatalf("Expected ok result but got: %+v", result)
	}

	if c0Verifier == "" || c1Verifier == "" {
		t.Fatalf("Failed to get verifier code c0=%q c1=%q", c0Verifier, c1Verifier)
	}

	if c0Verifier != c1Verifier {
		t.Fatalf("Expected verifiers to match but were different")
	}
}

func TestSendRecvEmptyFileDirect(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()

	DefaultTransitRelayURL = "tcp://"

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {

			relayServer := newRelayServer()
			defer relayServer.close()

			url := rs.WebSocketURL()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayServer.url.String()

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayServer.url.String()

			fileContent := make([]byte, 0)
			buf := bytes.NewReader(fileContent)

			code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, false)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, false)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, fileContent) {
				t.Fatalf("File contents mismatch")
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}
		})
	}
}

func TestSendRecvEmptyFileViaRelay(t *testing.T) {
	ctx := context.Background()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {

			rs := rendezvousservertest.NewServerLegacy()
			defer rs.Close()

			relayServer := newRelayServer()
			defer relayServer.close()

			url := rs.WebSocketURL()

			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayServer.url.String()

			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayServer.url.String()

			fileContent := make([]byte, 0)

			buf := bytes.NewReader(fileContent)

			code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, fileContent) {
				t.Fatalf("File contents mismatch")
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}
		})
	}
}

func TestWormholeDirectoryTransportSendRecvRelay(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			defer relayServer.close()
			relayURL := relayServer.url.String()

			var c0Verifier string
			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL
			c0.VerifierOk = func(code string) bool {
				c0Verifier = code
				return true
			}

			var c1Verifier string
			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURL
			c1.VerifierOk = func(code string) bool {
				c1Verifier = code
				return true
			}

			personalizeContent := make([]byte, 1<<16)
			for i := 0; i < len(personalizeContent); i++ {
				personalizeContent[i] = byte(i)
			}

			bodiceContent := []byte("placarding-whereat")

			entries := []DirectoryEntry{
				{
					Path: filepath.Join("skyjacking", "personalize.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(personalizeContent)
						return ioutil.NopCloser(b), nil
					},
				},
				{
					Path: filepath.Join("skyjacking", "bodice-Maytag.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(bodiceContent)
						return ioutil.NopCloser(b), nil
					},
				},
			}

			code, resultCh, err := c0.SendDirectory(ctx, "skyjacking", entries, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			r, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range r.File {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, err := ioutil.ReadAll(rc)
				if err != nil {
					t.Fatal(err)
				}
				rc.Close()

				if f.Name == "personalize.txt" {
					if !bytes.Equal(body, personalizeContent) {
						t.Fatal("personalize.txt file content does not match")
					}
				} else if f.Name == "bodice-Maytag.txt" {
					if !bytes.Equal(bodiceContent, body) {
						t.Fatalf("bodice-Maytag.txt file content does not match %s vs %s", bodiceContent, body)
					}
				} else {
					t.Fatalf("Unexpected file %s", f.Name)
				}
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}

			if c0Verifier == "" || c1Verifier == "" {
				t.Fatalf("Failed to get verifier code c0=%q c1=%q", c0Verifier, c1Verifier)
			}

			if c0Verifier != c1Verifier {
				t.Fatalf("Expected verifiers to match but were different")
			}
		})
	}
}

// Test when pairs when on relay fails to connect
func TestWormholeDirectoryTransportSendRecvRelayOneFail(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	// set url which we cannot connect from one side
	var relayFailURL = ("ws://localhost:40000")

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			defer relayServer.close()
			relayURL := relayServer.url.String()

			var c0Verifier string
			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL
			c0.VerifierOk = func(code string) bool {
				c0Verifier = code
				return true
			}

			var c1Verifier string
			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayFailURL
			c1.VerifierOk = func(code string) bool {
				c1Verifier = code
				return true
			}

			personalizeContent := make([]byte, 1<<16)
			for i := 0; i < len(personalizeContent); i++ {
				personalizeContent[i] = byte(i)
			}

			bodiceContent := []byte("placarding-whereat")

			entries := []DirectoryEntry{
				{
					Path: filepath.Join("skyjacking", "personalize.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(personalizeContent)
						return ioutil.NopCloser(b), nil
					},
				},
				{
					Path: filepath.Join("skyjacking", "bodice-Maytag.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(bodiceContent)
						return ioutil.NopCloser(b), nil
					},
				},
			}

			code, resultCh, err := c0.SendDirectory(ctx, "skyjacking", entries, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			r, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range r.File {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, err := ioutil.ReadAll(rc)
				if err != nil {
					t.Fatal(err)
				}
				rc.Close()

				if f.Name == "personalize.txt" {
					if !bytes.Equal(body, personalizeContent) {
						t.Fatal("personalize.txt file content does not match")
					}
				} else if f.Name == "bodice-Maytag.txt" {
					if !bytes.Equal(bodiceContent, body) {
						t.Fatalf("bodice-Maytag.txt file content does not match %s vs %s", bodiceContent, body)
					}
				} else {
					t.Fatalf("Unexpected file %s", f.Name)
				}
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}

			if c0Verifier == "" || c1Verifier == "" {
				t.Fatalf("Failed to get verifier code c0=%q c1=%q", c0Verifier, c1Verifier)
			}

			if c0Verifier != c1Verifier {
				t.Fatalf("Expected verifiers to match but were different")
			}
		})
	}
}

// Test when pairs use different valid relay services
func TestWormholeDirectoryTransportSendRecvTwoRelays(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()
			relayServerSecond := newRelayServer()
			defer relayServer.close()
			defer relayServerSecond.close()

			relayURL := relayServer.url.String()
			relayURLSecond := relayServerSecond.url.String()

			var c0Verifier string
			var c0 Client
			c0.RendezvousURL = url
			c0.TransitRelayURL = relayURL
			c0.VerifierOk = func(code string) bool {
				c0Verifier = code
				return true
			}

			var c1Verifier string
			var c1 Client
			c1.RendezvousURL = url
			c1.TransitRelayURL = relayURLSecond
			c1.VerifierOk = func(code string) bool {
				c1Verifier = code
				return true
			}

			personalizeContent := make([]byte, 1<<16)
			for i := 0; i < len(personalizeContent); i++ {
				personalizeContent[i] = byte(i)
			}

			bodiceContent := []byte("placarding-whereat")

			entries := []DirectoryEntry{
				{
					Path: filepath.Join("skyjacking", "personalize.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(personalizeContent)
						return ioutil.NopCloser(b), nil
					},
				},
				{
					Path: filepath.Join("skyjacking", "bodice-Maytag.txt"),
					Reader: func() (io.ReadCloser, error) {
						b := bytes.NewReader(bodiceContent)
						return ioutil.NopCloser(b), nil
					},
				},
			}

			code, resultCh, err := c0.SendDirectory(ctx, "skyjacking", entries, true)
			if err != nil {
				t.Fatal(err)
			}

			receiver, err := c1.Receive(ctx, code, true)
			if err != nil {
				t.Fatal(err)
			}

			got, err := ioutil.ReadAll(receiver)
			if err != nil {
				t.Fatal(err)
			}

			r, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range r.File {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, err := ioutil.ReadAll(rc)
				if err != nil {
					t.Fatal(err)
				}
				rc.Close()

				if f.Name == "personalize.txt" {
					if !bytes.Equal(body, personalizeContent) {
						t.Fatal("personalize.txt file content does not match")
					}
				} else if f.Name == "bodice-Maytag.txt" {
					if !bytes.Equal(bodiceContent, body) {
						t.Fatalf("bodice-Maytag.txt file content does not match %s vs %s", bodiceContent, body)
					}
				} else {
					t.Fatalf("Unexpected file %s", f.Name)
				}
			}

			result := <-resultCh
			if !result.OK {
				t.Fatalf("Expected ok result but got: %+v", result)
			}

			if c0Verifier == "" || c1Verifier == "" {
				t.Fatalf("Failed to get verifier code c0=%q c1=%q", c0Verifier, c1Verifier)
			}

			if c0Verifier != c1Verifier {
				t.Fatalf("Expected verifiers to match but were different")
			}
		})
	}
}

type testRelayServer struct {
	*httptest.Server
	l       net.Listener
	url     *url.URL
	proto   string
	wg      sync.WaitGroup
	mu      sync.Mutex
	streams map[string]net.Conn
}

func newTestTCPRelayServer() *testRelayServer {
	l, err := net.Listen("tcp4", ":0")
	if err != nil {
		panic(err)
	}

	url, err := url.Parse("tcp://" + l.Addr().String())
	if err != nil {
		panic(err)
	}
	rs := &testRelayServer{
		l:       l,
		url:     url,
		proto:   "tcp",
		streams: make(map[string]net.Conn),
	}

	go rs.run()
	return rs
}

func (ts *testRelayServer) close() {
	ts.l.Close()
	ts.wg.Wait()
}

func (ts *testRelayServer) run() {
	for {
		conn, err := ts.l.Accept()
		if err != nil {
			return
		}

		ts.wg.Add(1)
		go ts.handleConn(conn)
	}
}

func newTestWSRelayServer() *testRelayServer {
	rs := &testRelayServer{
		proto:   "ws",
		streams: make(map[string]net.Conn),
	}

	smux := http.NewServeMux()
	smux.HandleFunc("/", rs.handleWSRelay)

	rs.Server = httptest.NewServer(smux)
	url, err := url.Parse("ws://" + rs.Server.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	rs.url = url
	rs.l = rs.Server.Listener

	return rs
}

func (rs *testRelayServer) handleWSRelay(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)

	if err != nil {
		return
	}

	ctx := context.Background()
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	rs.wg.Add(1)
	go rs.handleConn(conn)
}

var headerPrefix = []byte("please relay ")
var headerSide = []byte(" for side ")

func (ts *testRelayServer) handleConn(c net.Conn) {
	// requests look like:
	// "please relay 10bf5ab71e48a3ca74b0a0d4d54f66f38704a76d15885442a8df141680fd for side 4a74cb8a377c970a\n"

	defer ts.wg.Done()
	headerBuf := make([]byte, 64)

	matchExpect := func(expect []byte) bool {
		got := headerBuf[:len(expect)]
		_, err := io.ReadFull(c, got)
		if err != nil {
			c.Close()
			return false
		}

		if !bytes.Equal(got, expect) {
			c.Write([]byte("bad handshake\n"))
			c.Close()
			return false
		}

		return true
	}

	isHex := func(str string) bool {
		_, err := hex.DecodeString(str)
		if err != nil {
			c.Write([]byte("bad handshake\n"))
			c.Close()
			return false
		}
		return true
	}

	if !matchExpect(headerPrefix) {
		return
	}

	_, err := io.ReadFull(c, headerBuf)
	if err != nil {
		c.Close()
		return
	}

	chanID := string(headerBuf)
	if !isHex(chanID) {
		return
	}

	if !matchExpect(headerSide) {
		return
	}

	sideBuf := headerBuf[:16]
	_, err = io.ReadFull(c, sideBuf)
	if err != nil {
		c.Close()
		return
	}

	side := string(sideBuf)
	if !isHex(side) {
		return
	}

	// read \n
	_, err = io.ReadFull(c, headerBuf[:1])
	if err != nil {
		c.Close()
		return
	}

	ts.mu.Lock()
	existing, found := ts.streams[chanID]
	if !found {
		ts.streams[chanID] = c
	}
	ts.mu.Unlock()

	if found {
		existing.Write([]byte("ok\n"))
		c.Write([]byte("ok\n"))
		go func() {
			io.Copy(c, existing)
			existing.Close()
			c.Close()

		}()

		io.Copy(existing, c)
		c.Close()
		existing.Close()
	}
}

type splitReader struct {
	*bytes.Reader
	offset    int
	cancelAt  int
	cancel    func()
	didCancel bool
}

func (s *splitReader) Read(b []byte) (int, error) {
	n, err := s.Reader.Read(b)
	s.offset += n
	if !s.didCancel && s.offset >= s.cancelAt {
		s.cancel()
		s.didCancel = true
		// yield the cpu to give the cancellation goroutine a chance
		// to run (esp important for when GOMAXPROCS=1)
		time.Sleep(1 * time.Millisecond)
	}
	return n, err
}

func TestClient_relayURL_default(t *testing.T) {
	var c Client

	DefaultTransitRelayURL = "tcp://transit.magic-wormhole.io:8001"
	url, err := c.relayURL()
	if err != nil {
		t.Error(err)
		return
	}
	if url.Scheme != "tcp" {
		t.Error(fmt.Sprintf("invalid protocol, expected tcp, got %v", url))
	}
}

func TestClient_relayURL_backwards_compatibility(t *testing.T) {
	var c Client

	DefaultTransitRelayURL = "tcp:transit.magic-wormhole.io:8001"
	url, err := c.relayURL()
	if err != nil {
		t.Error(err)
		return
	}
	if url.String() != "tcp://transit.magic-wormhole.io:8001" {
		t.Error(fmt.Sprintf("invalid URL, expected 'tcp://[…]', got %v", url))
	}
}

func TestWormholeFileTransportSendRecvViaWSRelayServer(t *testing.T) {
	ctx := context.Background()

	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	relayServer := newTestWSRelayServer()
	relayURL := relayServer.url.String()
	defer relayServer.close()

	var c0 Client
	c0.RendezvousURL = url
	c0.TransitRelayURL = relayURL

	var c1 Client
	c1.RendezvousURL = url
	c1.TransitRelayURL = relayURL

	fileContent := make([]byte, 1<<16)
	for i := 0; i < len(fileContent); i++ {
		fileContent[i] = byte(i)
	}

	buf := bytes.NewReader(fileContent)

	code, resultCh, err := c0.SendFile(ctx, "file.txt", buf, true)
	if err != nil {
		t.Fatal(err)
	}

	receiver, err := c1.Receive(ctx, code, true)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ioutil.ReadAll(receiver)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, fileContent) {
		t.Fatalf("File contents mismatch")
	}

	result := <-resultCh
	if !result.OK {
		t.Fatalf("Expected ok result but got: %+v", result)
	}
}

func TestReceiveWithoutSender(t *testing.T) {
	rs := rendezvousservertest.NewServerLegacy()
	defer rs.Close()

	url := rs.WebSocketURL()

	for relayProtocol, newRelayServer := range relayServerConstructors {
		t.Run(fmt.Sprintf("With %s relay server", relayProtocol), func(t *testing.T) {
			relayServer := newRelayServer()

			c0 := Client{
				RendezvousURL:   url,
				TransitRelayURL: relayServer.url.String(),
			}

			_, err := c0.Receive(context.Background(), "666-fireball-fallacy", false)

			if err == nil {
				t.Error("Receiver should not be able to claim the nameplate")
			}

			if err.Error() != "Nameplate is unclaimed: 666" {
				t.Error(fmt.Sprintf("Unexpected error: %s", err.Error()))
			}
		})
	}
}
