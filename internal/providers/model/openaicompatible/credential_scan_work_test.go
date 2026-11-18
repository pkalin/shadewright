/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openaicompatible

import (
	"strconv"
	"strings"
	"testing"
)

// maxWorkPerByte bounds the scan's work count per byte of reply: every byte is decoded, viewed and searched
// a fixed number of times, at most once per level of nested JSON text.
const maxWorkPerByte = 32

// nestedString encodes text as a JSON string depth times.
func nestedString(text string, depth int) string {
	for range depth {
		text = strconv.Quote(text)
	}
	return text
}

// workShapes builds replies of about n bytes that cost the scan the most per byte: many tiny strings, many
// object keys, deep arrays, JSON text nested in strings (deeper than the scan decodes), escape sequences
// cut short, and strings that start like JSON and are not.
func workShapes(n int) map[string]string {
	tiny := `{"choices":[{"message":{"content":"x"}}],"x":[` + strings.Repeat(`"a",`, n/4) + `"a"]}`
	keys := `{"choices":[],"x":{` + strings.Repeat(`"k":1,`, n/6) + `"k":1}}`
	deep := `{"choices":[],"x":` + strings.Repeat("[", min(n/2, 9000)) + strings.Repeat("]", min(n/2, 9000)) + `}`
	inner := `{"a":[` + strings.Repeat(`"sk-",`, n/48) + `"x"]}`
	nested := `{"choices":[{"message":{"content":` + nestedString(inner, 5) + `,"tool_calls":[{"function":{"arguments":` + nestedString(inner, 1) + `}}]}}]}`
	esc := `{"choices":[{"message":{"content":"` + strings.Repeat(`\u001b]\u001b[`, n/12) + `"}}]}`
	almost := `{"choices":[{"message":{"content":"x"}}],"x":[` + strings.Repeat(`"{\"a\":[1,2,3,4,5,6,7,8,9,10",`, n/32) + `"x"]}`
	parts := `{"output":[{"type":"message","content":[` + strings.Repeat(`{"type":"output_text","text":"{\"a\":\"b\"}"},`, n/48) + `{"type":"output_text","text":"x"}]}]}`
	return map[string]string{"tiny": tiny, "keys": keys, "deep": deep, "nested": nested, "escapes": esc, "almost-json": almost, "parts": parts}
}

// Property: the scan's work count stays under a fixed multiple of the reply's size for every shape, at two
// sizes, so it grows linearly.
func TestCredentialScanWorkIsLinear(t *testing.T) {
	for _, n := range []int{1 << 16, 1 << 19} {
		for name, reply := range workShapes(n) {
			for _, api := range []API{APIChatCompletions, APIResponses} {
				_, work := scanReplyWork([]byte(reply), api, secretKey)
				per := float64(work) / float64(len(reply))
				if per > maxWorkPerByte {
					t.Errorf("%s/%s at %d bytes: %d work, %.1f per byte", name, api, len(reply), work, per)
				}
				t.Logf("%s/%s: %d bytes, %.1f work per byte", name, api, len(reply), per)
			}
		}
	}
}
