package contracts

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCheckedResourceAccountingAcceptsExactBoundaries(t *testing.T) {
	tests := []struct {
		code     string
		resource string
	}{
		{code: DiagnosticCodeNamedInputMaxBytes, resource: "named input bytes"},
		{code: DiagnosticCodeNamedInputTotalMaxBytes, resource: "aggregate named input bytes"},
		{code: DiagnosticCodeRepositoryInventoryMaxFiles, resource: "repository inventory files"},
		{code: DiagnosticCodeRepositoryInventoryMaxBytes, resource: "repository inventory bytes"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			total, err := CheckedAddResource(7, 3, 10, test.code, test.resource)
			if err != nil || total != 10 {
				t.Fatalf("exact boundary = %d, %v", total, err)
			}
			_, err = CheckedAddResource(7, 4, 10, test.code, test.resource)
			var limitErr *ResourceLimitError
			if !errors.As(err, &limitErr) || limitErr.Code != test.code ||
				limitErr.Observed != 11 || limitErr.Limit != 10 ||
				!strings.Contains(err.Error(), "observed 11, limit 10") {
				t.Fatalf("limit error = %#v, %v", limitErr, err)
			}
		})
	}
}

func TestCheckedResourceAccountingRejectsInt64Overflow(t *testing.T) {
	_, err := CheckedAddResource(
		math.MaxInt64,
		1,
		math.MaxInt64,
		DiagnosticCodeRepositoryInventoryMaxBytes,
		"repository inventory bytes",
	)
	var accountingErr *ResourceLimitError
	if !errors.As(err, &accountingErr) ||
		accountingErr.Code != DiagnosticCodeResourceAccountingOverflow ||
		accountingErr.Current != math.MaxInt64 ||
		accountingErr.Increment != 1 ||
		!strings.Contains(err.Error(), "overflowed int64") {
		t.Fatalf("overflow error = %#v, %v", accountingErr, err)
	}
}
