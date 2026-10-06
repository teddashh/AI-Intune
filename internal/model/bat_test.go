package model

import (
	"strconv"
	"strings"
	"testing"
)

func TestBATServerConstantsConsistency(t *testing.T) {
	expectedCommand := "bat-remote auth 127.0.0.1:" + strconv.Itoa(BATServerPort)
	if BATServerEndpointCommand != expectedCommand {
		t.Errorf("BATServerEndpointCommand = %q, want %q", BATServerEndpointCommand, expectedCommand)
	}
	if !strings.Contains(BATServerEndpointCommand, "127.0.0.1") {
		t.Errorf("BATServerEndpointCommand missing 127.0.0.1")
	}
}
