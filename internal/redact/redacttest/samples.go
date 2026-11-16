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

// Package redacttest holds, for tests only, one sample secret per built-in rule of package redact, in rule
// order. Tests of other packages that place secrets around their own text (the context builder's cut of a
// value, #796) take them from here, so a new rule reaches them without a hand-picked list. Package redact's
// own test fails when a rule has no sample here, or when a sample is not redacted by its rule alone.
package redacttest

// Sample is a text that one built-in rule redacts.
type Sample struct {
	// Rule is the index of the rule in package redact's built-in list.
	Rule int
	// Key is the text before the secret that makes the rule match it ("token="), empty when the secret
	// matches on its own shape.
	Key string
	// Secret is the part that must never appear in redacted text.
	Secret string
	// Text is the sample as a whole: Key, Secret, and for some rules text around them. It may hold line
	// ends; a rule anchored to the start of a line matches at the start of Text.
	Text string
}

// Samples returns one sample per built-in rule, in the order of the rules.
//
// The secrets are synthetic: none of them was ever issued.
func Samples() []Sample {
	const value, pem = "s3cr3tValue99", "-----BEGIN RSA PRIVATE KEY-----\n"
	s := []Sample{
		{Key: pem, Secret: "MIIEpAIBAAKCAQEA7xVb9Lq3", Text: pem + "MIIEpAIBAAKCAQEA7xVb9Lq3\n-----END RSA PRIVATE KEY-----"},
		{Secret: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N"},
		{Secret: "ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5"},
		{Secret: "Xy18Q~aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456_"},
		{Key: "postgres://", Secret: "admin:s3cretPassw0rd@db-host:5432/app"},
		{Key: "Server=tcp:db;Password=", Secret: "Sup3rS3cret!x"},
		{Key: "Authorization: ", Secret: "Bearer abcDEF123456ghiJKL789"},
		{Key: "using bearer ", Secret: "Zm9vYmFyMTIzNDU2Nzg5MGFiY2Q"},
		{Key: "https://store.example.net/c?sv=2020&sig=", Secret: "AbCdEf123456GhIjKl%3D"},
		{Key: "token=", Secret: value},
		{Key: "--client-secret ", Secret: value},
		{Key: "aws_secret_access_key ", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{Key: `{"password":"`, Secret: "s3cr3t-Value99", Text: `{"password":"s3cr3t-Value99"}`},
		{Key: `{"name":"DB_PASSWORD","value":"`, Secret: "s3cr3t-Value99",
			Text: `{"name":"DB_PASSWORD","value":"s3cr3t-Value99"}`},
		{Key: "password: ", Secret: value},
		{Key: "password: |\n  ", Secret: value},
		{Key: "credentials:\n  user: ", Secret: value},
		{Key: "- name: DB_PASSWORD\n  value: ", Secret: value},
		{Secret: "Zx9Qw8Er7Ty6Ui5Op4As3Df2Gh1Jk0Lz"},
		{Secret: "jane.doe@example.com"},
		{Secret: "10.20.30.40"},
		{Secret: "2001:db8:85a3::8a2e:370:7334"},
	}
	for i := range s {
		s[i].Rule = i
		if s[i].Text == "" {
			s[i].Text = s[i].Key + s[i].Secret
		}
	}
	return s
}
