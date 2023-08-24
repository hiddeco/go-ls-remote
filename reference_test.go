package lsremote

import "testing"

func TestObjectID_Equals(t *testing.T) {
	oid1 := ObjectID("2ef7bde608ce5404e97d5f042f95f89f1c232871")
	oid2 := ObjectID("2ef7bde608ce5404e97d5f042f95f89f1c232871")
	oid3 := ObjectID("4b0bc2b3d7b1db1c63d8c8238ba2ef7bde608ce5")

	if !oid1.Equals(oid2) {
		t.Errorf("Expected oid1 to equal oid2")
	}

	if oid1.Equals(oid3) {
		t.Errorf("Expected oid1 to not equal oid3")
	}
}

func TestObjectID_IsUnborn(t *testing.T) {
	unborn := UnbornRef
	oid := ObjectID("2ef7bde608ce5404e97d5f042f95f89f1c232871")

	if !unborn.IsUnborn() {
		t.Errorf("Expected UnbornRef to be considered unborn")
	}

	if oid.IsUnborn() {
		t.Errorf("Expected oid to not be considered unborn")
	}
}

func TestObjectID_String(t *testing.T) {
	oid := ObjectID("2ef7bde608ce5404e97d5f042f95f89f1c232871")
	expectedString := "2ef7bde608ce5404e97d5f042f95f89f1c232871"

	if oid.String() != expectedString {
		t.Errorf("Expected %s, but got %s", expectedString, oid.String())
	}
}

func TestSymRefTarget_Equals(t *testing.T) {
	symRef1 := SymRefTarget("refs/heads/main")
	symRef2 := SymRefTarget("refs/heads/main")
	symRef3 := SymRefTarget("refs/heads/develop")

	if !symRef1.Equals(string(symRef2)) {
		t.Errorf("Expected symRef1 to equal symRef2")
	}

	if symRef1.Equals(string(symRef3)) {
		t.Errorf("Expected symRef1 to not equal symRef3")
	}
}

func TestSymRefTarget_String(t *testing.T) {
	symRef := SymRefTarget("refs/heads/main")
	expectedString := "refs/heads/main"

	if symRef.String() != expectedString {
		t.Errorf("Expected %s, but got %s", expectedString, symRef.String())
	}
}
