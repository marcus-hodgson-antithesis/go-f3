package f3

import (
	"bytes"
	"fmt"
)

// A ticket is a signature over some common payload.
type Ticket []byte

func (t Ticket) Compare(other Ticket) int {
	return bytes.Compare(t, other)
}

// Computes VRF tickets for use in CONVERGE phase.
// A VRF ticket is produced by signing a payload which digests a beacon randomness value and
// the instance and round numbers.
type VRFTicketSource interface {
	MakeTicket(beacon []byte, instance int, round int, signer string) Ticket
}

type VRFTicketVerifier interface {
	VerifyTicket(beacon []byte, instance int, round int, signer string, ticket Ticket) bool
}

type FakeVRF struct {
}

func NewFakeVRF() *FakeVRF {
	return &FakeVRF{}
}

func (f *FakeVRF) MakeTicket(beacon []byte, instance int, round int, signer string) Ticket {
	return []byte(fmt.Sprintf("FakeTicket(%x, %d, %d, %s)", beacon, instance, round, signer))
}

func (f *FakeVRF) VerifyTicket(beacon []byte, instance int, round int, signer string, ticket Ticket) bool {
	return string(ticket) == fmt.Sprintf("FakeTicket(%x, %d, %d, %s)", beacon, instance, round, signer)
}