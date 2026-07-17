package auth

import "testing"

func TestArgon2IDHashRoundTrip(t *testing.T) {
	t.Parallel()

	const secret = "a-long-cryptographically-random-test-key"
	hash, err := HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if hash == secret {
		t.Fatal("API key was stored in plaintext")
	}
	if !VerifySecret(secret, hash) {
		t.Fatal("correct secret did not verify")
	}
	if VerifySecret(secret+"-wrong", hash) {
		t.Fatal("incorrect secret verified")
	}
}

func TestCapabilitiesAreNotImplicitlyExpanded(t *testing.T) {
	t.Parallel()

	reader := NewPrincipal("reader", []string{ReadDocuments, Search}, false, "")
	if !reader.Has(ReadDocuments, Search) {
		t.Fatal("explicit capabilities were not recognized")
	}
	if reader.Has(WriteDocuments) {
		t.Fatal("read-only key unexpectedly gained write_documents")
	}
	admin := NewPrincipal("admin", []string{Admin}, false, "")
	if !admin.Has(WriteDocuments, WriteGraph, ManageConflicts) {
		t.Fatal("admin capability should authorize all operations")
	}
}
