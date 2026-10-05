package runner

import "testing"

const simList = `{"devices":{
 "com.apple.CoreSimulator.SimRuntime.iOS-17-5":[{"udid":"A","name":"iPhone 15","isAvailable":true}],
 "com.apple.CoreSimulator.SimRuntime.iOS-26-2":[
   {"udid":"B","name":"iPad Pro","isAvailable":true},
   {"udid":"C","name":"iPhone 17","isAvailable":true},
   {"udid":"D","name":"iPhone 16e","isAvailable":false}],
 "com.apple.CoreSimulator.SimRuntime.watchOS-11-0":[{"udid":"W","name":"iPhone-like","isAvailable":true}]}}`

func TestPickSimulatorPrefersNewestRuntimeAvailableIPhone(t *testing.T) {
	got, err := pickSimulator([]byte(simList))
	if err != nil || got.UDID != "C" || got.Runtime != "26.2" {
		t.Fatalf("pickSimulator() = %#v, %v", got, err)
	}
}

func TestPickSimulatorErrors(t *testing.T) {
	if _, err := pickSimulator([]byte(`{"devices":{}}`)); err == nil {
		t.Fatal("expected error without simulators")
	}
	if _, err := pickSimulator([]byte(`nope`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestFilterOutDestinationKeepsContainerAndScheme(t *testing.T) {
	got := filterOutDestination([]string{"-project", "A.xcodeproj", "-scheme", "S", "-destination", "x", "-derivedDataPath", "/d", "CODE_SIGNING_ALLOWED=NO", "-showdestinations"})
	want := []string{"-project", "A.xcodeproj", "-scheme", "S", "-showdestinations"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
