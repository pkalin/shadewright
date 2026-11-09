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
// `kubectl shadewright status` for an overview, `kubectl shadewright explain <name>` for why one request (or
// each request of one Incident) is in its phase. It runs with the caller's kubeconfig and RBAC, and only
// reads (get and list): the Incidents and RemediationRequests of the operator namespace, the
// RemediationSettings and the analysis budget ConfigMap, and the version label of its CRDs, to warn when they
// come from another release than the plugin (`kubectl shadewright version` prints the plugin's). Every string
// from the cluster, which includes text the model wrote, is cleaned of control characters and terminal escape
// sequences before it is printed (invariant 4), and a pull request link is printed only for the Git repository
// configured in RemediationSettings. A troubleshooting link is a fixed URL chosen by the reason, never built
// from cluster text.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
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
  kubectl shadewright explain <request-or-incident> [flags]
  kubectl shadewright version

Commands:
  status    Incidents, requests, the day's analysis budget and the kill switch
  explain   why one RemediationRequest is in its phase: its condition timeline, Incident, action, policy
            decision, plan digest, pull request and handoffs, and for a stop the troubleshooting section;
            given an Incident's name, the same for each of its requests, newest first

Flags:
  -n, --namespace string      operator namespace (default: the kubeconfig context's namespace)
  -A, --all-namespaces        read every namespace (status only)
  -o, --output string         text or json (default text)
      --kubeconfig string     path to the kubeconfig file
      --context string        kubeconfig context to use
      --request-timeout dur   time limit for all reads, as kubectl takes it: seconds ("30"), a duration
                              ("30s", "1m") or 0 for no limit (default 30s)
`

// The commands that read the cluster.
const (
	cmdStatus  = "status"
	cmdExplain = "explain"
)

// The output formats.
const (
	outputText = "text"
	outputJSON = "json"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", clean(err.Error(), maxMessage))
		os.Exit(1)
	}
}

// cmdFlags are the flags of the status and explain commands.
type cmdFlags struct {
	namespace, output, kubeconfig, context string
	allNamespaces                          bool
	timeout                                time.Duration
	// name is the argument of explain: a RemediationRequest or Incident name.
	name string
}

// parseStatus parses the flags of the status command. It prints nothing: an error is returned once, for main
// to print, and -h or --help returns flag.ErrHelp.
func parseStatus(args []string) (cmdFlags, error) {
	return parseFlags(cmdStatus, args)
}

// parseExplain parses the arguments of the explain command: one name, before or after the flags, and the
// flags of status without -A.
func parseExplain(args []string) (cmdFlags, error) {
	return parseFlags(cmdExplain, args)
}

// parseFlags parses the flags of cmd. Explain takes exactly one name, which may stand between flags as with
// kubectl; status takes none.
func parseFlags(cmd string, args []string) (cmdFlags, error) {
	var f cmdFlags
	var timeout string
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	for _, n := range []string{"n", "namespace"} {
		fs.StringVar(&f.namespace, n, "", "operator namespace")
	}
	if cmd == cmdStatus {
		for _, n := range []string{"A", "all-namespaces"} {
			fs.BoolVar(&f.allNamespaces, n, false, "read every namespace")
		}
	}
	for _, n := range []string{"o", "output"} {
		fs.StringVar(&f.output, n, outputText, "text or json")
	}
	fs.StringVar(&f.kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	fs.StringVar(&f.context, "context", "", "kubeconfig context")
	fs.StringVar(&timeout, "request-timeout", "30s", "time limit for all reads")
	var names []string
	for {
		if err := fs.Parse(args); err != nil {
			return f, err
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		args = fs.Args()[1:]
	}
	var err error
	if f.timeout, err = parseTimeout(timeout); err != nil {
		return f, err
	}
	switch {
	case cmd == cmdStatus && len(names) > 0:
		return f, fmt.Errorf("unexpected argument %q", clean(names[0], maxName))
	case cmd == cmdExplain && len(names) == 0:
		return f, errors.New("explain needs the name of a RemediationRequest or an Incident")
	case cmd == cmdExplain && len(names) > 1:
		return f, fmt.Errorf("unexpected argument %q: explain takes one name", clean(names[1], maxName))
	case f.output != outputText && f.output != outputJSON:
		return f, fmt.Errorf("--output must be text or json, not %q", clean(f.output, maxName))
	}
	if len(names) == 1 {
		f.name = names[0]
	}
	return f, nil
}

// parseTimeout reads --request-timeout as kubectl does: a whole number of seconds, or a Go duration with a
// unit; 0 means no limit. A negative value is refused.
func parseTimeout(s string) (time.Duration, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 || n > math.MaxInt64/int64(time.Second) {
			return 0, fmt.Errorf("--request-timeout must be 0 or a positive number of seconds, not %q", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--request-timeout must be seconds or a duration such as 30s or 1m, not %q", s)
	}
	return d, nil
}

// crdCheckTimeout bounds the CRD version check, which runs next to the reads of the status and must never
// hold them up.
const crdCheckTimeout = 5 * time.Second

// run executes the command line args (without the program name).
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(stdout, usage)
		return nil
	}
	if args[0] == "version" {
		if len(args) > 1 {
			return fmt.Errorf("unexpected argument %q", clean(args[1], maxName))
		}
		_, err := fmt.Fprintln(stdout, "kubectl-shadewright", clean(version, 63))
		return err
	}
	var f cmdFlags
	var err error
	switch args[0] {
	case cmdStatus:
		f, err = parseStatus(args[1:])
	case cmdExplain:
		f, err = parseExplain(args[1:])
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", clean(args[0], maxName))
	}
	if errors.Is(err, flag.ErrHelp) {
		_, err = fmt.Fprint(stdout, usage)
		return err
	}
	if err != nil {
		return err
	}

	c, namespace, err := newClient(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if f.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
	}
	defer cancel()
	now := time.Now()
	// The version check runs next to the reads, with its own short limit, so a slow CRD read cannot use up
	// the time of the other reads.
	warning := make(chan string, 1)
	go func() {
		cctx, ccancel := context.WithTimeout(ctx, crdCheckTimeout)
		defer ccancel()
		warning <- crdVersionWarning(cctx, c, version)
	}()
	var out any
	if args[0] == cmdExplain {
		out, err = explain(ctx, client.Reader(c), namespace, f.name)
	} else {
		out, err = collect(ctx, client.Reader(c), Options{Namespace: namespace, AllNamespaces: f.allNamespaces}, now)
	}
	if w := <-warning; w != "" {
		_, _ = fmt.Fprintln(stderr, w)
	}
	if err != nil {
		return err
	}
	if f.output == outputJSON {
		return renderJSON(stdout, out)
	}
	if e, ok := out.(*Explanation); ok {
		return renderExplainText(stdout, e, now)
	}
	return renderText(stdout, out.(*Report), now)
}

// newClient builds the client from the kubeconfig, with the context, namespace and timeout of f, and returns
// it with the namespace to read.
func newClient(f cmdFlags) (client.Client, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = f.kubeconfig
	kc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: f.context, Context: clientcmdapi.Context{Namespace: f.namespace}})
	cfg, err := kc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("loading the kubeconfig: %w", err)
	}
	namespace, _, err := kc.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("reading the namespace of the kubeconfig context: %w", err)
	}
	cfg.Timeout = f.timeout
	cfg.UserAgent = "kubectl-shadewright"

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("creating the client: %w", err)
	}
	return c, namespace, nil
}
