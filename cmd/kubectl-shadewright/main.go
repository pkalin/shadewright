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

// Command kubectl-shadewright is a kubectl plugin that shows what Shadewright is doing in a cluster:
// `kubectl shadewright status`. It runs with the caller's kubeconfig and RBAC, and only reads (get and list):
// the Incidents and RemediationRequests of the operator namespace, the RemediationSettings and the analysis
// budget ConfigMap, and the version label of its CRDs, to warn when they come from another release than the
// plugin (`kubectl shadewright version` prints the plugin's). Every string from the cluster, which includes
// text the model wrote, is cleaned of control characters and terminal escape sequences before it is printed
// (invariant 4), and a pull request link is printed only for the Git repository configured in
// RemediationSettings.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const usage = `kubectl shadewright: read-only view of Shadewright in a cluster.

Usage:
  kubectl shadewright status [flags]
  kubectl shadewright version

Flags:
  -n, --namespace string      operator namespace (default: the kubeconfig context's namespace)
  -A, --all-namespaces        read every namespace
  -o, --output string         text or json (default text)
      --kubeconfig string     path to the kubeconfig file
      --context string        kubeconfig context to use
      --request-timeout dur   time limit for all reads (default 30s)
`

// The output formats.
const (
	outputText = "text"
	outputJSON = "json"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "Error:", clean(err.Error(), maxMessage))
		}
		os.Exit(1)
	}
}

// statusFlags are the flags of the status command.
type statusFlags struct {
	namespace, output, kubeconfig, context string
	allNamespaces                          bool
	timeout                                time.Duration
}

// parseStatus parses the flags of the status command.
func parseStatus(args []string, stderr io.Writer) (statusFlags, error) {
	var f statusFlags
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	for _, n := range []string{"n", "namespace"} {
		fs.StringVar(&f.namespace, n, "", "operator namespace")
	}
	for _, n := range []string{"A", "all-namespaces"} {
		fs.BoolVar(&f.allNamespaces, n, false, "read every namespace")
	}
	for _, n := range []string{"o", "output"} {
		fs.StringVar(&f.output, n, outputText, "text or json")
	}
	fs.StringVar(&f.kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	fs.StringVar(&f.context, "context", "", "kubeconfig context")
	fs.DurationVar(&f.timeout, "request-timeout", 30*time.Second, "time limit for all reads")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	switch {
	case fs.NArg() > 0:
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case f.output != outputText && f.output != outputJSON:
		return f, fmt.Errorf("--output must be text or json, not %q", f.output)
	case f.timeout <= 0:
		return f, errors.New("--request-timeout must be positive")
	}
	return f, nil
}

// run executes the command line args (without the program name).
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(stdout, usage)
		return nil
	}
	if args[0] == "version" {
		if len(args) > 1 {
			return fmt.Errorf("unexpected argument %q", args[1])
		}
		_, err := fmt.Fprintln(stdout, "kubectl-shadewright", clean(version, 63))
		return err
	}
	if args[0] != "status" {
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
	f, err := parseStatus(args[1:], stderr)
	if err != nil {
		return err
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = f.kubeconfig
	kc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: f.context, Context: clientcmdapi.Context{Namespace: f.namespace}})
	cfg, err := kc.ClientConfig()
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}
	namespace, _, err := kc.Namespace()
	if err != nil {
		return fmt.Errorf("reading the namespace of the kubeconfig context: %w", err)
	}
	cfg.Timeout = f.timeout
	cfg.UserAgent = "kubectl-shadewright"

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating the client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	now := time.Now()
	if w := crdVersionWarning(ctx, c, version); w != "" {
		_, _ = fmt.Fprintln(stderr, w)
	}
	report, err := collect(ctx, client.Reader(c), Options{Namespace: namespace, AllNamespaces: f.allNamespaces}, now)
	if err != nil {
		return err
	}
	if f.output == outputJSON {
		return renderJSON(stdout, report)
	}
	return renderText(stdout, report, now)
}
