package plumbing

import (
	"errors"
	"testing"
)

func TestPortReserveConflictIsAtomic(t *testing.T) {
	a := newPortAllocator()
	err := a.Reserve("c1", []PortMapping{
		{HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
		{HostPort: 9090, ContainerPort: 90, Proto: "tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// c2 asks for one free and one taken port: must fail entirely.
	err = a.Reserve("c2", []PortMapping{
		{HostPort: 7070, ContainerPort: 70, Proto: "tcp"},
		{HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
	})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("expected ErrPortInUse, got %v", err)
	}
	if _, ok := a.Owner("tcp", 7070); ok {
		t.Fatal("failed reservation must not leave partial state")
	}
	if owner, _ := a.Owner("tcp", 8080); owner != "c1" {
		t.Fatal("existing owner must be untouched")
	}
}

func TestPortProtoAndValidation(t *testing.T) {
	a := newPortAllocator()
	// Same port number on different protocols does not conflict.
	if err := a.Reserve("c1", []PortMapping{{HostPort: 53, ContainerPort: 53, Proto: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reserve("c2", []PortMapping{{HostPort: 53, ContainerPort: 53, Proto: "udp"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []PortMapping{
		{HostPort: 0, ContainerPort: 80, Proto: "tcp"},
		{HostPort: 70000, ContainerPort: 80, Proto: "tcp"},
		{HostPort: 80, ContainerPort: 0, Proto: "tcp"},
		{HostPort: 80, ContainerPort: 80, Proto: "sctp"},
	} {
		if err := a.Reserve("c3", []PortMapping{bad}); !errors.Is(err, ErrInvalidPortMap) {
			t.Fatalf("expected ErrInvalidPortMap for %+v, got %v", bad, err)
		}
	}
}

func TestPortReleaseAll(t *testing.T) {
	a := newPortAllocator()
	_ = a.Reserve("c1", []PortMapping{{HostPort: 8000, ContainerPort: 80, Proto: "tcp"}})
	_ = a.Reserve("c2", []PortMapping{{HostPort: 8001, ContainerPort: 80, Proto: "tcp"}})
	a.ReleaseAll("c1")
	if _, ok := a.Owner("tcp", 8000); ok {
		t.Fatal("released port still owned")
	}
	if owner, _ := a.Owner("tcp", 8001); owner != "c2" {
		t.Fatal("release must not affect other containers")
	}
	// Port is reusable after release.
	if err := a.Reserve("c3", []PortMapping{{HostPort: 8000, ContainerPort: 80, Proto: "tcp"}}); err != nil {
		t.Fatal(err)
	}
}
