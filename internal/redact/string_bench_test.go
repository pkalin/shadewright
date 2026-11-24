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

package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// benchLog returns log lines of four common shapes, about n bytes.
func benchLog(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, "2026-11-21T10:00:%02d.123Z INFO server started listening on :8080 request_id=%d path=/api/v1/items\n", i%60, i)
		case 1:
			fmt.Fprintf(&b, "level=error msg=\"connection refused\" host=db-%d.default.svc port=5432 retry=%d\n", i, i%7)
		case 2:
			fmt.Fprintf(&b, "{\"ts\":%d,\"level\":\"warn\",\"msg\":\"slow query\",\"duration_ms\":%d,\"user\":\"svc-%d\"}\n", i, i*3, i)
		default:
			fmt.Fprintf(&b, "panic: runtime error: index out of range [%d] with length %d\n\tat main.handler(main.go:%d)\n", i, i-1, i)
		}
	}
	return b.String()
}

// benchManifest returns a Deployment manifest of about n bytes.
func benchManifest(n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\n  labels:\n    app: web\nspec:\n  template:\n    spec:\n      containers:\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "      - name: c%d\n        image: registry.example.com/team/app-%d:1.2.%d\n        env:\n        - name: LOG_LEVEL\n          value: info\n        - name: DB_HOST\n          value: db-%d.shop.svc\n        resources:\n          limits:\n            memory: 256Mi\n", i, i, i, i)
	}
	return b.String()
}

// BenchmarkRedactString measures String per byte on a short value, logs and a manifest with no secret in them, the
// common case: compare it with main (benchstat) when a change touches the search of the rules (#873).
func BenchmarkRedactString(b *testing.B) {
	r, err := redact.New()
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		text string
	}{
		{"summary-80B", "Back-off restarting failed container web in pod web-7d9f8c-x2x (OOMKilled)"},
		{"log-4KiB", benchLog(4 << 10)},
		{"log-64KiB", benchLog(64 << 10)},
		{"manifest-16KiB", benchManifest(16 << 10)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.text)))
			b.ReportAllocs()
			for b.Loop() {
				_ = r.String(tc.text)
			}
		})
	}
}
