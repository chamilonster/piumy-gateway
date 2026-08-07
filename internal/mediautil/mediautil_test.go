package mediautil

import (
	"encoding/base64"
	"testing"
)

func TestDecodeDataURL(t *testing.T) {
	raw, mime, err := DecodeDataURL("data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("hola")))
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || string(raw) != "hola" {
		t.Errorf("got mime=%q raw=%q, want image/png / hola", mime, raw)
	}
	if _, _, err := DecodeDataURL("not-a-data-url"); err == nil {
		t.Error("DecodeDataURL on garbage input: want an error")
	}
	if _, _, err := DecodeDataURL("data:image/pngbase64nocomma"); err == nil {
		t.Error("DecodeDataURL with a prefix but no comma: want an error")
	}
	if _, _, err := DecodeDataURL("data:image/png;base64,not-valid-base64!!"); err == nil {
		t.Error("DecodeDataURL with invalid base64 payload: want an error")
	}
}
