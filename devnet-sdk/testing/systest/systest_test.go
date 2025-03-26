package systest

import (
	"context"
	"fmt"
	"testing"

	"github.com/ethereum-optimism/optimism/devnet-sdk/shell/env"
	"github.com/ethereum-optimism/optimism/devnet-sdk/system"
	"github.com/stretchr/testify/require"
)

// mockSystemTestHelper is a test implementation of systemTestHelper
type mockSystemTestHelper struct {
	expectPreconditionsMet bool
	systemTestCalls        int
	interopTestCalls       int
	preconditionErrors     []error
	systemAcquirer         func() (system.System, error)
}

func (h *mockSystemTestHelper) handlePreconditionError(t BasicT, err error) {
	h.preconditionErrors = append(h.preconditionErrors, err)
	if h.expectPreconditionsMet {
		t.Fatalf("%v", &PreconditionError{err: err})
	} else {
		t.Skipf("%v", &PreconditionError{err: err})
	}
}

func (h *mockSystemTestHelper) AcquireSystem(t BasicT, validators ...PreconditionValidator) (T, system.System) {
	h.systemTestCalls++
	wt := NewT(t)

	ctx, cancel := context.WithCancel(wt.Context())
	defer cancel()
	wt = wt.WithContext(ctx)

	sys, err := h.systemAcquirer()
	if err != nil {
		h.handlePreconditionError(t, err)
		return nil, nil
	}

	for _, validator := range validators {
		ctx, err := validator(wt, sys)
		if err != nil {
			h.handlePreconditionError(t, err)
			return nil, nil
		}
		wt = wt.WithContext(ctx)
	}

	return wt, sys
}

func (h *mockSystemTestHelper) AcquireInteropSystem(t BasicT, validators ...PreconditionValidator) (T, system.InteropSystem) {
	h.interopTestCalls++
	wt, sys := h.AcquireSystem(t, validators...)
	if sys, ok := sys.(system.InteropSystem); ok {
		return wt, sys
	} else {
		h.handlePreconditionError(t, fmt.Errorf("interop test requested, but system is not an interop system"))
	}
	return nil, nil
}

// mockEnvGetter implements envGetter for testing
type mockEnvGetter struct {
	values map[string]string
}

func (g mockEnvGetter) Getenv(key string) string {
	return g.values[key]
}

// TestSystemTestHelper tests the basic implementation of systemTestHelper
func TestSystemTestHelper(t *testing.T) {
	t.Run("newBasicSystemTestHelper initialization", func(t *testing.T) {
		testCases := []struct {
			name     string
			envValue string
			want     bool
		}{
			{"empty env", "", false},
			{"invalid value", "invalid", false},
			{"zero", "0", false},
			{"false", "false", false},
			{"FALSE", "FALSE", false},
			{"False", "False", false},
			{"f", "f", false},
			{"F", "F", false},
			{"one", "1", true},
			{"true", "true", true},
			{"TRUE", "TRUE", true},
			{"True", "True", true},
			{"t", "t", true},
			{"T", "T", true},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				env := mockEnvGetter{
					values: map[string]string{
						env.ExpectPreconditionsMet: tc.envValue,
					},
				}
				helper := newBasicSystemTestHelper(env)
				require.Equal(t, tc.want, helper.expectPreconditionsMet)
			})
		}
	})
}

// TestSystemTest tests the main SystemTest function
func TestSystemTest(t *testing.T) {
	t.Run("system acquisition failure", func(t *testing.T) {
		testCases := []struct {
			name        string
			expectMet   bool
			expectSkip  bool
			expectFatal bool
		}{
			{
				name:        "preconditions not expected skips test",
				expectMet:   false,
				expectSkip:  true,
				expectFatal: false,
			},
			{
				name:        "preconditions expected fails test",
				expectMet:   true,
				expectSkip:  false,
				expectFatal: true,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				helper := &mockSystemTestHelper{
					expectPreconditionsMet: tc.expectMet,
					systemAcquirer: func() (system.System, error) {
						return nil, fmt.Errorf("failed to acquire system")
					},
				}

				recorder := &mockTBRecorder{mockTB: mockTB{name: "test"}}
				wt, sys := helper.AcquireSystem(recorder)
				require.Nil(t, wt, "should not return wt")
				require.Nil(t, sys, "should not return system")

				require.Equal(t, tc.expectSkip, recorder.skipped, "unexpected skip state")
				require.Equal(t, tc.expectFatal, recorder.failed, "unexpected fatal state")
				require.Len(t, helper.preconditionErrors, 1, "expected one precondition error")
				require.Contains(t, helper.preconditionErrors[0].Error(), "failed to acquire system")
			})
		}
	})

	t.Run("successful system acquisition", func(t *testing.T) {
		helper := &mockSystemTestHelper{
			systemAcquirer: func() (system.System, error) {
				return newMockSystem(), nil
			},
		}

		wt, sys := helper.AcquireSystem(t)
		require.NotNil(t, wt)
		require.NotNil(t, sys)
	})

	t.Run("with validator", func(t *testing.T) {
		helper := &mockSystemTestHelper{
			systemAcquirer: func() (system.System, error) {
				return newMockSystem(), nil
			},
		}

		validatorCalled := false

		validator := func(t T, sys system.System) (context.Context, error) {
			validatorCalled = true
			return t.Context(), nil
		}

		wt, sys := helper.AcquireSystem(t, validator)
		require.NotNil(t, wt)
		require.NotNil(t, sys)
		require.True(t, validatorCalled)
	})

	t.Run("multiple validators", func(t *testing.T) {
		helper := &mockSystemTestHelper{
			systemAcquirer: func() (system.System, error) {
				return newMockSystem(), nil
			},
		}

		validatorCount := 0
		validator := func(t T, sys system.System) (context.Context, error) {
			validatorCount++
			return t.Context(), nil
		}

		wt, sys := helper.AcquireSystem(t, validator, validator, validator)
		require.NotNil(t, wt)
		require.NotNil(t, sys)
		require.Equal(t, 3, validatorCount)
	})
}

// TestInteropSystemTest tests the InteropSystemTest function
func TestInteropSystemTest(t *testing.T) {
	t.Run("skips non-interop system", func(t *testing.T) {
		helper := &mockSystemTestHelper{
			systemAcquirer: func() (system.System, error) {
				return newMockSystem(), nil
			},
		}

		recorder := &mockTBRecorder{mockTB: mockTB{name: "test"}}
		wt, sys := helper.AcquireInteropSystem(recorder)
		require.Nil(t, wt, "should not return wt")
		require.Nil(t, sys, "should not return interop system")
		require.Len(t, helper.preconditionErrors, 1)
		require.Contains(t, helper.preconditionErrors[0].Error(), "interop test requested")
	})

	t.Run("runs with interop system", func(t *testing.T) {
		helper := &mockSystemTestHelper{
			systemAcquirer: func() (system.System, error) {
				return newMockInteropSystem(), nil
			},
		}

		wt, sys := helper.AcquireInteropSystem(t)
		require.NotNil(t, wt)
		require.NotNil(t, sys)
		require.NotNil(t, sys.InteropSet())
		require.Empty(t, helper.preconditionErrors)
	})
}

// TestPreconditionError tests the PreconditionError type and its behavior
func TestPreconditionError(t *testing.T) {
	t.Run("error wrapping", func(t *testing.T) {
		underlying := fmt.Errorf("test error")
		precondErr := &PreconditionError{err: underlying}

		require.Equal(t, "precondition not met: test error", precondErr.Error())
		require.ErrorIs(t, precondErr, underlying)
	})
}

// TestPreconditionHandling tests the precondition error handling behavior
func TestPreconditionHandling(t *testing.T) {
	testCases := []struct {
		name        string
		expectMet   bool
		expectSkip  bool
		expectFatal bool
	}{
		{
			name:        "preconditions not expected skips test",
			expectMet:   false,
			expectSkip:  true,
			expectFatal: false,
		},
		{
			name:        "preconditions expected fails test",
			expectMet:   true,
			expectSkip:  false,
			expectFatal: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			helper := &mockSystemTestHelper{
				expectPreconditionsMet: tc.expectMet,
				systemAcquirer: func() (system.System, error) {
					return newMockSystem(), nil
				},
			}

			recorder := &mockTBRecorder{mockTB: mockTB{name: "test"}}
			testErr := fmt.Errorf("test precondition error")

			wt, sys := helper.AcquireSystem(recorder, func(t T, sys system.System) (context.Context, error) {
				return t.Context(), testErr
			})
			require.Nil(t, wt)
			require.Nil(t, sys)

			require.Equal(t, tc.expectSkip, recorder.skipped, "unexpected skip state")
			require.Equal(t, tc.expectFatal, recorder.failed, "unexpected fatal state")
			require.Len(t, helper.preconditionErrors, 1, "expected one precondition error")
			require.Equal(t, testErr, helper.preconditionErrors[0])

			if tc.expectSkip {
				require.Contains(t, recorder.skipMsg, "precondition not met")
			}
			if tc.expectFatal {
				require.Contains(t, recorder.fatalMsg, "precondition not met")
			}
		})
	}
}
