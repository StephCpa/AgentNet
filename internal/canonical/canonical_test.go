package canonical

import "testing"

func TestRejectMalformedUTF8AndDuplicateKeys(t *testing.T) {
	for _, raw := range [][]byte{[]byte("{\"s\":\"\xff\"}"), []byte(`{"a":1,"\u0061":2}`), []byte(`{"s":"\ud800"}`), []byte(`{"n":9007199254740992}`)} {
		if _, err := Bytes(raw); err == nil {
			t.Fatalf("accepted invalid bytes %x", raw)
		}
	}
}

func TestUTF16OrderAndNumbers(t *testing.T) {
	raw := []byte(`{"\ue000":1.0,"\ud83d\ude00":-0.0}`)
	got, err := Bytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"😀\":0,\"\ue000\":1}" {
		t.Fatalf("got %s", got)
	}
}
