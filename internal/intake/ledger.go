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
)

// Reservation is one model call counted at its maximum before it is made (ADR-0047 section 6). Its name holds
// everything the month's sum needs, so the sum is read from a listing and never from a body.
type Reservation struct {
	// Month is the intake's own month, "2006-01", read from its clock once at the start of a run.
	Month string
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
	// Spent returns the sum of the tokens of the month's reservations of the family. A failed read is an
	// error, never zero.
	Spent(ctx context.Context, month string, family Family) (int64, error)
	// Reserve creates the reservation, create-only; ErrReservationExists when its name exists.
	Reserve(ctx context.Context, r Reservation) error
}

var (
	monthPattern       = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
	runPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	reservationPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}\.([1-3])\.([0-9]{1,12})\.json$`)
)

// reservationDir returns the directory of the month's reservations of the family under the ledger root,
// "spend/<month>/<family>", built from checked parts only; a reservation in it is "<run>.<call>.<tokens>.json".
func reservationDir(root, month string, family Family) (string, error) {
	if !monthPattern.MatchString(month) {
		return "", errors.New("the month is not of the form 2006-01")
	}
	if family != FamilyAnthropic && family != FamilyOpenAI {
		return "", errors.New("the family is not one the intake knows")
	}
	return filepath.Join(root, "spend", month, string(family)), nil
}

// DirLedger keeps reservations as empty files in a local directory, in the layout of the scores container.
type DirLedger struct {
	Root string
}

// Spent sums the tokens in the names of the month's reservations of the family. A directory that does not
// exist holds none. A name that is not a reservation's is an error: the sum cannot be trusted.
func (l DirLedger) Spent(ctx context.Context, month string, family Family) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dir, err := reservationDir(l.Root, month, family)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("listing the spend reservations: %w", err)
	}
	var sum int64
	for _, e := range entries {
		m := reservationPattern.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			return 0, errors.New("the spend directory holds an entry that is not a reservation")
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return 0, errors.New("a spend reservation has a token count out of range")
		}
		sum += n
	}
	return sum, nil
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
	if !runPattern.MatchString(r.Run) || r.Call < 1 || r.Call > 3 || r.Tokens < 0 || r.Tokens > 999_999_999_999 {
		return errors.New("the reservation has an invalid run, call or token count")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the spend directory: %w", err)
	}
	name := r.Run + "." + strconv.Itoa(r.Call) + "." + strconv.FormatInt(r.Tokens, 10) + ".json"
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
