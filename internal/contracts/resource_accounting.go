package contracts

import (
	"fmt"
	"math"
)

const (
	DiagnosticCodeNamedInputMaxBytes          = "named_input_max_bytes_exceeded"
	DiagnosticCodeNamedInputTotalMaxBytes     = "named_input_total_max_bytes_exceeded"
	DiagnosticCodeRepositoryInventoryMaxFiles = "repository_inventory_max_files_exceeded"
	DiagnosticCodeRepositoryInventoryMaxBytes = "repository_inventory_max_bytes_exceeded"
	DiagnosticCodeResourceAccountingOverflow  = "resource_accounting_overflow"
)

// ResourceLimitError is the stable, content-free contract shared by named
// input and repository accounting. Callers may wrap it in their own diagnostic
// phase while retaining the code and actionable numeric details.
type ResourceLimitError struct {
	Code      string
	Resource  string
	Limit     int64
	Observed  int64
	Current   int64
	Increment int64
}

func (e *ResourceLimitError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == DiagnosticCodeResourceAccountingOverflow {
		return fmt.Sprintf(
			"%s accounting overflowed int64 while adding %d to %d",
			e.Resource,
			e.Increment,
			e.Current,
		)
	}
	return fmt.Sprintf(
		"%s exceeds limit: observed %d, limit %d",
		e.Resource,
		e.Observed,
		e.Limit,
	)
}

// CheckResourceLimit accepts the exact boundary and returns a typed error when
// the observed amount exceeds its positive ceiling.
func CheckResourceLimit(code string, resource string, observed int64, limit int64) error {
	if observed < 0 {
		return &ResourceLimitError{
			Code:     DiagnosticCodeResourceAccountingOverflow,
			Resource: resource,
			Observed: observed,
			Limit:    limit,
		}
	}
	if observed <= limit {
		return nil
	}
	return &ResourceLimitError{
		Code:     code,
		Resource: resource,
		Limit:    limit,
		Observed: observed,
	}
}

// CheckedAddResource performs non-negative int64 accumulation before applying
// a ceiling. Overflow and limit failures are distinguishable by stable codes.
func CheckedAddResource(current int64, increment int64, limit int64, limitCode string, resource string) (int64, error) {
	if current < 0 || increment < 0 || current > math.MaxInt64-increment {
		return 0, &ResourceLimitError{
			Code:      DiagnosticCodeResourceAccountingOverflow,
			Resource:  resource,
			Limit:     limit,
			Current:   current,
			Increment: increment,
		}
	}
	total := current + increment
	if err := CheckResourceLimit(limitCode, resource, total, limit); err != nil {
		if typed, ok := err.(*ResourceLimitError); ok {
			typed.Current = current
			typed.Increment = increment
		}
		return 0, err
	}
	return total, nil
}
