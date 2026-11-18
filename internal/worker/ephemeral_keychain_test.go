package worker

import (
	"reflect"
	"testing"
)

func TestParseKeychainList(t *testing.T) {
	input := "  \"/tmp/job.keychain-db\"\n  \"/Users/test/Library/Keychains/login.keychain-db\"\n"
	want := []string{"/tmp/job.keychain-db", "/Users/test/Library/Keychains/login.keychain-db"}
	if got := parseKeychainList(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseKeychainList() = %#v, want %#v", got, want)
	}
}

func TestRandomSecret(t *testing.T) {
	first, err := randomSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomSecret(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 || len(second) != 64 || first == second {
		t.Fatalf("unexpected random secrets: lengths %d and %d", len(first), len(second))
	}
}
