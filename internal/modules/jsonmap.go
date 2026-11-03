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

package modules

import (
	"encoding/json"
	"math"
)

// JSONInteger reads a JSON integer from an object as decoded by encoding/json (float64 or json.Number) or by
// the Kubernetes decoder (int64). It reports false for anything else, a fraction or an out-of-range number
// included. Symptom sources and verification checks use it on untrusted objects.
func JSONInteger(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// MaxConditions is how many entries of status.conditions [FirstCondition] reads. The API sets no limit, so a
// forged object with a huge list cannot make a source do unbounded work; real objects carry a handful.
const MaxConditions = 64

// FirstCondition returns the first entry of status.conditions whose type is typ, among the first
// [MaxConditions], or nil when status, its conditions or such an entry are missing or not of the JSON shape
// expected. status is the object's status as decoded from JSON and is only read.
func FirstCondition(status map[string]any, typ string) map[string]any {
	conds, _ := status["conditions"].([]any)
	for i, c := range conds {
		if i == MaxConditions {
			break
		}
		cond, _ := c.(map[string]any)
		if t, _ := cond["type"].(string); t == typ {
			return cond
		}
	}
	return nil
}
