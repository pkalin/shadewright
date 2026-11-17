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

// Package openaicompatible calls an operator-allowed HTTPS model endpoint through the shared OpenAI wire
// formats (ADR-0042). It receives no token source. A separate optional key is read from a bounded regular
// file per attempt and bound to an exact install-time host. No key means no Authorization header. Redirects
// and proxies are disabled. Only named cluster Services may resolve to private unicast addresses; all other
// endpoints require public addresses. The analyzer enforces the suffix allow-list before construction.
// Responses without usable usage are refused and charged at the loop's upper bound, never zero.
// Errors and call reports contain fixed text, never credentials, paths, prompts or server-controlled text.
package openaicompatible
