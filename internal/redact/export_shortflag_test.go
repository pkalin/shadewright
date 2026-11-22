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

package redact

import "math/rand/v2"

// ShortContainer is a container's command and arguments with a password flag in them (#1075), the secret its
// value holds and the words that must stay.
type ShortContainer struct {
	Form          string
	Command, Args []string
	Secret        string
	Kept          []string
}

// ShortContainers returns containers whose command is a tool of the table and whose arguments hold its password
// flag in every form the tool reads it, the same after "sh -c", and containers with no command (the entrypoint
// in the image) whose arguments hold a flag of the arguments with no command word.
func ShortContainers() []ShortContainer {
	rng := rand.New(rand.NewPCG(1075, 4)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []ShortContainer
	i := 0
	for _, t := range shortTools {
		for _, f := range shortArgForms {
			if f.separated && t.glued {
				continue
			}
			secret := shortSecret(rng, i)
			i++
			args := append(append([]string{shortHead, "9"}, f.words(t, shortValue(t, secret))...), shortTail)
			out = append(out,
				ShortContainer{Form: "command " + f.name, Command: []string{t.name}, Args: args, Secret: secret,
					Kept: []string{t.name, shortHead, shortTail}},
				ShortContainer{Form: "sh -c " + f.name, Command: []string{"/bin/sh", "-c"},
					Args: []string{shellJoin(t.name, args)}, Secret: secret, Kept: []string{t.name, shortHead, shortTail}})
		}
	}
	for _, t := range shortArgsFlags {
		for _, f := range shortArgForms[:3] {
			secret := shortSecret(rng, i)
			i++
			out = append(out,
				ShortContainer{Form: "args " + f.name, Args: append(f.words(t, shortValue(t, secret)), shortTail),
					Secret: secret, Kept: []string{shortTail}},
				ShortContainer{Form: "args after a flag " + f.name,
					Args:   append(append([]string{"--log-level", "debug"}, f.words(t, shortValue(t, secret))...), shortTail),
					Secret: secret, Kept: []string{"--log-level", shortTail}})
		}
	}
	return out
}
