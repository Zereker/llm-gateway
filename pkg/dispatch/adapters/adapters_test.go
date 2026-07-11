package adapters

import (
	"testing"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/selector"
)

func TestClassMappings(t *testing.T) {
	cases := []struct {
		dispatch dispatch.Class
		selector selector.ErrorClass
		invoker  invoker.Class
	}{
		{dispatch.ClassSuccess, selector.ClassSuccess, invoker.ClassSuccess},
		{dispatch.ClassTransient, selector.ClassTransient, invoker.ClassTransient},
		{dispatch.ClassCapacity, selector.ClassCapacity, invoker.ClassCapacity},
		{dispatch.ClassPermanent, selector.ClassPermanent, invoker.ClassPermanent},
		{dispatch.ClassInvalid, selector.ClassInvalid, invoker.ClassInvalid},
		{dispatch.ClassUnknown, selector.ClassUnknown, invoker.ClassUnknown},
	}
	for _, tc := range cases {
		if got := dispatchClassToSelector(tc.dispatch); got != tc.selector {
			t.Errorf("dispatchClassToSelector(%v) = %v", tc.dispatch, got)
		}
		if got := invokerClassToDispatch(tc.invoker); got != tc.dispatch {
			t.Errorf("invokerClassToDispatch(%v) = %v", tc.invoker, got)
		}
	}
}
