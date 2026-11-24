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

package released

import (
	"sync"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// exprs holds the meter.Expr of each rule's expression, made on its first read: one per built-in rule and one per
// extra pattern of each redactor New made, which the components make once at their start.
var exprs sync.Map // *regexp.Regexp (a rule's re) -> *meter.Expr

// findAll returns ru.re.FindAllStringSubmatchIndex(s, -1), charging to wm each byte the engine reads, attempt by
// attempt (meter.Expr): an expression that reads the rest of the text on every match attempt (a.*z|a on a run of
// a) costs the square of the text, and the budget stops it there instead of after the call (#873, decision of
// 2026-11-20). The release counted the text once (counted); the matches are the same.
func (ru *rule) findAll(wm *meter.Meter, s string) [][]int {
	e, ok := exprs.Load(ru.re)
	if !ok {
		e, _ = exprs.LoadOrStore(ru.re, meter.NewExpr(ru.re))
	}
	return e.(*meter.Expr).FindAll(s, func(n int) { tick(wm, n) })
}
