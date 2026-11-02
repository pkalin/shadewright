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

package providers

import "context"

// Notification is a message for a human. Key identifies the thread: notifying with the same key again
// updates the existing thread instead of starting a new one.
type Notification struct {
	// Key is stable per subject, for example the RemediationRequest UID.
	Key   string
	Title string
	Body  string
}

// NotificationRef points at the thread a notification went to.
type NotificationRef struct {
	ID  string
	URL string
}

// Notifier tells a human that something needs attention (ADR-0014).
type Notifier interface {
	// Notify creates the thread for the key or updates it. Repeating a call never creates a second thread.
	Notify(ctx context.Context, n Notification) (NotificationRef, error)
}
