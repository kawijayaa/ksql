package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseKeyValuesDocumentedModes(t *testing.T) {
	for _, tc := range []struct {
		value   string
		options KeyValueOptions
		want    map[string]string
	}{
		{"ThreadId:458745723, Machine:Node001, Text: The service is up, Level: Info", KeyValueOptions{Mode: "delimited", Pair: ",", KV: ":", Keys: []string{"Text", "ThreadId", "Machine"}},
			map[string]string{"Text": "The service is up", "ThreadId": "458745723", "Machine": "Node001"}},
		{`src=10.1.1.123 dst=10.1.1.124 bytes=125 failure="connection aborted" "event time"=2021-01-01T10:00:54`, KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Quotes: []string{`"`}, Keys: []string{"event time", "src", "dst", "bytes", "failure"}},
			map[string]string{"event time": "2021-01-01T10:00:54", "src": "10.1.1.123", "dst": "10.1.1.124", "bytes": "125", "failure": "connection aborted"}},
		{`src=10.1.1.123 dst=10.1.1.124 bytes=125 failure=(connection aborted) (event time)=(2021-01-01 10:00:54)`, KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Quotes: []string{"()"}, Keys: []string{"event time", "failure"}},
			map[string]string{"event time": "2021-01-01 10:00:54", "failure": "connection aborted"}},
		{`src=10.1.1.123 failure="the remote host sent \"bye!\"" time=2021-01-01T10:00:54`, KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Quotes: []string{`"`}, Escapes: []string{`\`}, Keys: []string{"failure", "time"}},
			map[string]string{"failure": `the remote host sent "bye!"`, "time": "2021-01-01T10:00:54"}},
		{"name=John Doe phone=555 5555 city=New York", KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Keys: []string{"name", "phone", "city"}},
			map[string]string{"name": "John", "phone": "555", "city": "New"}},
		{"name=John Doe phone=555 5555 city=New York", KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Greedy: true, Keys: []string{"name", "phone", "city"}},
			map[string]string{"name": "John Doe", "phone": "555 5555", "city": "New York"}},
		{"2021-01-01T10:00:34 [INFO] ThreadId:458745723, Machine:Node001, Text: Started", KeyValueOptions{Mode: "free", Keys: []string{"Text", "ThreadId", "Machine"}},
			map[string]string{"Text": "Started", "ThreadId": "458745723", "Machine": "Node001"}},
		{`2021-01-01T10:00:34 [INFO] ThreadId:458745723, Machine:Node001, Text: 'The service \' is up'`, KeyValueOptions{Mode: "free", Quotes: []string{"'"}, Escapes: []string{`\`}, Keys: []string{"Text", "ThreadId", "Machine"}},
			map[string]string{"Text": "The service ' is up", "ThreadId": "458745723", "Machine": "Node001"}},
		{`["referer url: https://hostname.com/redirect?dest=/?h=1234", "request url: https://hostname.com/?h=1234", "advertiser id: 24fefbca-cf27-4d62-a623-249c2ad30c73"]`, KeyValueOptions{Mode: "regex", Regex: `"([\w ]+)\s*:\s*([^"]*)"`, Keys: []string{"referer url", "request url", "advertiser id"}},
			map[string]string{"referer url": "https://hostname.com/redirect?dest=/?h=1234", "request url": "https://hostname.com/?h=1234", "advertiser id": "24fefbca-cf27-4d62-a623-249c2ad30c73"}},
		// First appearance wins, unrequested keys and pairs without kv are ignored.
		{"a=1;b=2;a=3;junk;c", KeyValueOptions{Mode: "delimited", Pair: ";", KV: "=", Keys: []string{"a", "c"}}, map[string]string{"a": "1"}},
		{"Machine:Text, Text: Started", KeyValueOptions{Mode: "free", Keys: []string{"Machine", "Text"}}, map[string]string{"Machine": "Text", "Text": "Started"}},
		{"k=\"unterminated", KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Quotes: []string{`"`}, Keys: []string{"k"}}, map[string]string{"k": "unterminated"}},
		{"ключ=значение 😀=x", KeyValueOptions{Mode: "delimited", Pair: " ", KV: "=", Keys: []string{"ключ", "😀"}}, map[string]string{"ключ": "значение", "😀": "x"}},
		{"a:=:b", KeyValueOptions{Mode: "delimited", Pair: ":=", KV: ":", Keys: []string{"a"}}, map[string]string{"a": "=:b"}},
		{"", KeyValueOptions{Mode: "free", Keys: []string{"a"}}, map[string]string{}},
	} {
		got, err := ParseKeyValues(tc.value, tc.options)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: got %#v, %v; want %#v", tc.value, got, err, tc.want)
		}
	}
}

func TestKeyValuesJSONValidation(t *testing.T) {
	if got, err := KeyValuesJSON("a=1", `{"mode":"delimited","pair":" ","kv":"=","keys":["a"]}`); err != nil || got != `{"a":"1"}` {
		t.Fatalf("got %s, %v", got, err)
	}
	for _, options := range []string{`{`, `{"mode":"other","keys":[]}`, `{"mode":"delimited","pair":"","kv":"=","keys":[]}`, `{"mode":"regex","regex":"(a)","keys":[]}`, `{"mode":"free","quotes":["abc"],"keys":[]}`, `{"mode":"free","quotes":["'"],"escapes":["ab"],"keys":[]}`} {
		if _, err := KeyValuesJSON("a", options); err == nil {
			t.Fatalf("invalid options accepted: %s", options)
		}
	}
	if _, err := KeyValuesJSON(strings.Repeat("a", MaxKeyValueInputBytes+1), `{"mode":"free","keys":["a"]}`); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func FuzzParseKeyValues(f *testing.F) {
	f.Add("a=1 b=\"x y\" c=(z)", " ", "=", true)
	f.Add("k:v, k2: 'q\\'r'", ",", ":", false)
	f.Fuzz(func(t *testing.T, value, pair, kv string, greedy bool) {
		if len(value) > 4096 {
			return
		}
		for _, options := range []KeyValueOptions{
			{Mode: "delimited", Pair: pair, KV: kv, Greedy: greedy, Quotes: []string{`"`, "()"}, Escapes: []string{`\`}, Keys: []string{"a", "b", "k"}},
			{Mode: "free", Quotes: []string{"'"}, Escapes: []string{`\`}, Keys: []string{"a", "k"}},
		} {
			result, err := ParseKeyValues(value, options)
			if err != nil {
				continue
			}
			for key := range result {
				if key != "a" && key != "b" && key != "k" {
					t.Fatalf("unrequested key %q", key)
				}
			}
		}
	})
}
