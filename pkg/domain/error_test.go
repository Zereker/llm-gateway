package domain

import (
	"errors"
	"net/http"
	"testing"
)

func TestErrorClass_String(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want string
	}{
		{ErrUnknown, "unknown"},
		{ErrInvalid, "invalid"},
		{ErrPermanent, "permanent"},
		{ErrTransient, "transient"},
		{ErrRateLimit, "capacity"},
		{ErrorClass(999), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.c.String(); got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestDefaultHTTPStatus(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want int
	}{
		{ErrInvalid, http.StatusBadRequest},
		{ErrPermanent, http.StatusForbidden},
		{ErrRateLimit, http.StatusTooManyRequests},
		{ErrTransient, http.StatusBadGateway},
		{ErrUnknown, http.StatusInternalServerError},
		{ErrorClass(999), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.c.String(), func(t *testing.T) {
			if got := DefaultHTTPStatus(tc.c); got != tc.want {
				t.Errorf("got=%d, want=%d", got, tc.want)
			}
		})
	}
}

func TestAdapterError_Error(t *testing.T) {
	e := &AdapterError{Class: ErrInvalid, Message: "bad input"}
	if e.Error() != "bad input" {
		t.Errorf("Error()=%q", e.Error())
	}
}

func TestAdapterError_Unwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &AdapterError{Class: ErrTransient, Message: "wrap", Cause: cause}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is should match Cause")
	}
}

func TestAdapterError_Unwrap_NilCause(t *testing.T) {
	e := &AdapterError{Class: ErrInvalid, Message: "no cause"}
	if got := e.Unwrap(); got != nil {
		t.Errorf("Unwrap()=%v, want nil", got)
	}
}
