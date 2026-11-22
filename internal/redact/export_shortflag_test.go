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

import (
	"math/rand/v2"

	corev1 "k8s.io/api/core/v1"
)

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
					Args:   append(append([]string{shortLogLevel, shortDebug}, f.words(t, shortValue(t, secret))...), shortTail),
					Secret: secret, Kept: []string{shortLogLevel, shortTail}})
		}
	}
	return out
}

// ShortImagePod is a pod spec whose container of a table image holds its tool's password flag in its arguments,
// with no command (#1075, the image row), the secret and the words that must stay.
type ShortImagePod struct {
	Form   string
	Spec   corev1.PodSpec
	Secret string
	Kept   []string
}

// ShortImagePods returns, for every image of the table and every flag form its tool reads the password in, the
// container alone, before and after a container of another image whose "-p" is a port, and as an init and an
// ephemeral container next to it.
func ShortImagePods() []ShortImagePod {
	rng := rand.New(rand.NewPCG(1075, 6)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []ShortImagePod
	i := 0
	for _, row := range shortImageRows() {
		for _, f := range shortArgForms[:3] {
			if f.separated && row.tool.glued {
				continue
			}
			for _, p := range shortPlacements[:5] {
				secret := shortImageSecret(rng, row.tool, f)
				image := shortImageRefs[i%len(shortImageRefs)](row.image)
				i++
				args := append(append([]string{shortLogLevel, shortDebug}, f.words(row.tool, shortValue(row.tool, secret))...),
					shortTail)
				db := corev1.Container{Name: "db", Image: image, Args: args}
				app := corev1.Container{Name: "app", Image: shortAppImage, Args: shortAppArgs}
				kept := []string{shortLogLevel, shortTail}
				if p.name != "one container" {
					kept = append(kept, `"`+shortAppPort+`"`)
				}
				pod, _ := p.object(db, app).(*corev1.Pod)
				out = append(out, ShortImagePod{Form: row.image + " " + f.name + ", " + p.name, Spec: pod.Spec,
					Secret: secret, Kept: kept})
			}
		}
	}
	return out
}
