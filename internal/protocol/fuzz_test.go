package protocol

import (
	"encoding/json"
	"testing"
)

func FuzzDecode(f *testing.F) {
	seeds := []string{
		`{"v":1,"type":"room-join","data":{"code":"ABCD"}}`,
		`{"v":1,"type":"transfer-offer","data":{"id":"x","to":"p","name":"a","size":1}}`,
		`{"v":1,"type":"snippet","data":{"to":"p","text":"hi"}}`,
		`{"v":1,"type":"error"}`,
		`{"v":9999999999999999999,"type":"error"}`,
		`{"v":1,"type":""}`,
		`{"v":1,"type":"x","data":"not an object"}`,
		`{"v":1,"type":"x","data":[1,2,3]}`,
		`[]`, `{}`, `null`, `0`, `"str"`,
		"{\"v\":1,\"type\":\"\x00\"}",
		`{"v":1,"type":"` + string(make([]byte, 4096)) + `"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		env, err := Decode(raw)
		if err != nil {
			return
		}

		if env.V != Version || env.Type == "" {
			t.Fatalf("Decode accepted invalid envelope: %+v", env)
		}
		if _, err := json.Marshal(env); err != nil {
			t.Fatalf("accepted envelope does not re-marshal: %v", err)
		}
	})
}
