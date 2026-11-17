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

// Package openaiwire encodes Chat Completions and Responses and parses their untrusted replies.
// It owns no credentials. The Foundry and OpenAI-compatible adapters share these wire formats.
// Existing Foundry request bytes, parse results and fixed errors are pinned by the pre-extraction golden.
// The historical foundry error prefix is retained for compatibility; errors never quote input.
package openaiwire
