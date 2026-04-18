// Package assert provides runtime invariant checks for coord internals.
//
// Assertions trigger panics on failure — they signal programmer errors,
// not operating errors. Operating errors use coord's sentinel errors.
// See docs/invariants.md for the canonical invariant list.
package assert

import (
	"fmt"
	"reflect"
)

// Precondition panics if cond is false, formatting msg with args.
// Use for caller-contract checks at the start of a function.
func Precondition(cond bool, msg string, args ...any) {
	if cond {
		return
	}
	panicf(msg, args...)
}

// Postcondition panics if cond is false. Semantically identical to
// Precondition but marks the check as a return-time invariant.
func Postcondition(cond bool, msg string, args ...any) {
	if cond {
		return
	}
	panicf(msg, args...)
}

// NoError panics if err is non-nil. Use where an error would indicate
// internal corruption, not an operating failure the caller can handle.
func NoError(err error, msg string, args ...any) {
	if err == nil {
		return
	}
	if len(args) == 0 {
		panic("assert: " + msg + ": " + err.Error())
	}
	panic("assert: " + fmt.Sprintf(msg, args...) + ": " + err.Error())
}

// NotNil panics if v is nil (including typed-nil via reflect).
func NotNil(v any, msg string, args ...any) {
	if v == nil {
		panicf(msg, args...)
		return
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		if rv.IsNil() {
			panicf(msg, args...)
		}
	}
}

// NotEmpty panics if s is the empty string.
func NotEmpty(s string, msg string, args ...any) {
	if s != "" {
		return
	}
	panicf(msg, args...)
}

func panicf(msg string, args ...any) {
	if len(args) == 0 {
		panic("assert: " + msg)
	}
	panic("assert: " + fmt.Sprintf(msg, args...))
}
