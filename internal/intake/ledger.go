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

package intake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Reservation is one model call counted at its maximum before it is made (ADR-0047 section 6). Its name holds
// everything the month's sum and the count of a record's attempts need, so both are read from one listing and
// never from a body.
type Reservation struct {
	// Month is the intake's own month, "2006-01", read from its clock once at the start of a run.
	Month string
	// Day is the UTC day of the same clock read, "20060102".
	Day string
	// Record is the record ID, in the shape recordIDFromName checks and nothing else.
	Record string
	// Family is the provider family of the call.
	Family Family
	// Run identifies the run (the queue message ID and dequeue count in the job; a random ID on a laptop).
	Run string
	// Call is the call's index in the run: 1 the judge, 2 the cross-check, 3 the judge with the tool result.
	Call int
	// Tokens is the call's maximum.
	Tokens int64
}

// ErrReservationExists is returned by Ledger.Reserve for a name that exists: the same run and call reserved
// already. The run ends without a model call, as an error for a retry (ADR-0047 section 2, "What a 409 means").
var ErrReservationExists = errors.New("the spend reservation exists already")

// Ledger keeps the spend reservations: a directory on a laptop, the scores container in the job (#991).
type Ledger interface {
	// List returns the month's reservations of the family, read from their names. A failed read is an error,
	// never an empty list.
	List(ctx context.Context, month string, family Family) ([]Reservation, error)
	// Reserve creates the reservation, create-only; ErrReservationExists when its name exists.
	Reserve(ctx context.Context, r Reservation) error
}

// Spent returns the sum of the tokens of the reservations.
func Spent(rs []Reservation) int64 {
	var sum int64
	for _, r := range rs {
		sum += r.Tokens
	}
	return sum
}

// Attempts returns the number of distinct runs among the reservations of the record on the day: the record's
// attempts that day. An empty record counts for none.
func Attempts(rs []Reservation, day, record string) int {
	runs := map[string]bool{}
	for _, r := range rs {
		if r.Day == day && r.Record == record && record != "" {
			runs[r.Run] = true
		}
	}
	return len(runs)
}

var (
	monthPattern = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
	runPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	// reservationPattern is "<day>.<record ID>.<run>.<call>.<tokens>.json". The record ID is checked again
	// with recordIDFromName; none of the parts holds a dot.
	reservationPattern = regexp.MustCompile(`^([0-9]{8})\.([0-9a-z-]{1,64})\.([a-z0-9][a-z0-9-]{0,63})\.([1-3])\.([0-9]{1,12})\.json$`)
)

// dayLayout is the UTC day of a reservation's name.
const dayLayout = "20060102"

// maxTokens bounds the token count of a name: twelve digits.
const maxTokens = 999_999_999_999

// reservationDir returns the directory of the month's reservations of the family under the ledger root,
// "spend/<month>/<family>", built from checked parts only.
func reservationDir(root, month string, family Family) (string, error) {
	if !monthPattern.MatchString(month) {
		return "", errors.New("the month is not of the form 2006-01")
	}
	if family != FamilyAnthropic && family != FamilyOpenAI {
		return "", errors.New("the family is not one the intake knows")
	}
	return filepath.Join(root, "spend", month, string(family)), nil
}

// dayInMonth reports whether day is a UTC day "20060102" of month "2006-01".
func dayInMonth(day, month string) bool {
	t, err := time.Parse(dayLayout, day)
	return err == nil && t.Format(dayLayout) == day && t.Format("2006-01") == month
}

// checkedRecordID reports whether id is a record ID in the shape the name check gives, and nothing else.
func checkedRecordID(id string) bool {
	got, ok := recordIDFromName(id + ".json")
	return ok && got == id
}

// ReservationName returns the name of a reservation in its month and family directory,
// "<day>.<record ID>.<run>.<call>.<tokens>.json", built only from parts that pass their checks: the record ID
// in the shape of the name check, the day in the month, the run, the call and the token count.
func ReservationName(r Reservation) (string, error) {
	switch {
	case !monthPattern.MatchString(r.Month) || !dayInMonth(r.Day, r.Month):
		return "", errors.New("the reservation's day is not a day of its month")
	case !checkedRecordID(r.Record):
		return "", errors.New("the reservation's record is not a record ID")
	case !runPattern.MatchString(r.Run) || r.Call < 1 || r.Call > 3 || r.Tokens < 0 || r.Tokens > maxTokens:
		return "", errors.New("the reservation has an invalid run, call or token count")
	}
	return r.Day + "." + r.Record + "." + r.Run + "." + strconv.Itoa(r.Call) + "." +
		strconv.FormatInt(r.Tokens, 10) + ".json", nil
}

// parseReservation reads a name of the month's family directory, "<day>.<record ID>.<run>.<call>.<tokens>.json".
// It returns false for any other name, and for a day outside the month or a record that is not a record ID.
func parseReservation(name, month string, family Family) (Reservation, bool) {
	m := reservationPattern.FindStringSubmatch(name)
	if m == nil || !dayInMonth(m[1], month) || !checkedRecordID(m[2]) {
		return Reservation{}, false
	}
	r := Reservation{Month: month, Family: family, Day: m[1], Record: m[2], Run: m[3]}
	call, tokens := m[4], m[5]
	c, err := strconv.Atoi(call)
	if err != nil {
		return Reservation{}, false
	}
	n, err := strconv.ParseInt(tokens, 10, 64)
	if err != nil {
		return Reservation{}, false
	}
	r.Call, r.Tokens = c, n
	return r, true
}

// DirLedger keeps reservations as empty files in a local directory, in the layout of the scores container.
type DirLedger struct {
	Root string
}

// List reads the names of the month's reservations of the family. A directory that does not exist holds none.
// A name that does not parse is an error naming it, never skipped: neither the sum nor the count of attempts
// can be trusted. This holds for a name with no day or record as well.
func (l DirLedger) List(ctx context.Context, month string, family Family) ([]Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := reservationDir(l.Root, month, family)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the spend reservations: %w", err)
	}
	out := make([]Reservation, 0, len(entries))
	for _, e := range entries {
		r, ok := parseReservation(e.Name(), month, family)
		if !ok || !e.Type().IsRegular() {
			return nil, fmt.Errorf("the spend directory holds an entry that is not a reservation: %q", e.Name())
		}
		out = append(out, r)
	}
	return out, nil
}

// Reserve creates the reservation's file with O_EXCL.
func (l DirLedger) Reserve(ctx context.Context, r Reservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := reservationDir(l.Root, r.Month, r.Family)
	if err != nil {
		return err
	}
	name, err := ReservationName(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the spend directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- built from checked parts
	if errors.Is(err, fs.ErrExist) {
		return ErrReservationExists
	}
	if err != nil {
		return fmt.Errorf("creating the spend reservation: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("creating the spend reservation: %w", err)
	}
	return nil
}
