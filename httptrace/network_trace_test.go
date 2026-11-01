package httptrace

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestNetworkObservationHooksComposeWithCallerTrace(t *testing.T) {
	var events []string
	makeTrace := func(name string) *ClientTrace {
		return &ClientTrace{
			WriteHeadersStart: func(method string) { events = append(events, name+"-write-"+method) },
			GotResponseHeaders: func(method string, status int) {
				events = append(events, fmt.Sprintf("%s-headers-%s-%d", name, method, status))
			},
			GotResponseBodyDone: func(method string) { events = append(events, name+"-done-"+method) }, GotResponseBodyBytes: func(method string, n int) { events = append(events, fmt.Sprintf("%s-body-%s-%d", name, method, n)) },
		}
	}
	parent := WithClientTrace(context.Background(), makeTrace("parent"))
	ctx := WithClientTrace(parent, makeTrace("child"))
	trace := ContextClientTrace(ctx)
	trace.WriteHeadersStart("GET")
	trace.GotResponseHeaders("GET", 200)
	trace.GotResponseBodyBytes("GET", 17)
	trace.GotResponseBodyDone("GET")
	expected := []string{"child-write-GET", "parent-write-GET", "child-headers-GET-200", "parent-headers-GET-200", "child-body-GET-17", "parent-body-GET-17", "child-done-GET", "parent-done-GET"}
	if !reflect.DeepEqual(events, expected) {
		t.Fatal("lost caller hooks or arguments", events)
	}
}
