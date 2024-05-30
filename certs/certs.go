package certs

import (
	"bytes"
	"cmp"
	"slices"
	"sort"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-f3/gpbft"
	"golang.org/x/xerrors"
)

// PowerTableDelta represents a single power table change between GPBFT instances.
type PowerTableDelta struct {
	// Participant with changed power
	ParticipantID gpbft.ActorID
	// Change in power from base (signed).
	PowerDelta *gpbft.StoragePower
	// New signing key if relevant (else empty)
	SigningKey gpbft.PubKey
}

func (d *PowerTableDelta) IsZero() bool {
	return d.PowerDelta.Sign() == 0 && len(d.SigningKey) == 0
}

// FinalityCertificate represents a single finalized GPBFT instance.
type FinalityCertificate struct {
	// The GPBFT instance to which this finality certificate corresponds.
	GPBFTInstance uint64
	// The ECChain finalized during this instance, starting with the last tipset finalized in
	// the previous instance.
	ECChain gpbft.ECChain
	// Additional data signed by the participants in this instance. Currently used to certify
	// the power table used in the next instance.
	SupplementalData gpbft.SupplementalData
	// Indexes in the base power table of the certifiers (bitset)
	Signers bitfield.BitField
	// Aggregated signature of the certifiers
	Signature []byte
	// Changes between the power table used to validate this finality certificate and the power
	// used to validate the next finality certificate. Sorted by ParticipantID, ascending.
	PowerTableDelta []PowerTableDelta
}

// NewFinalityCertificate constructs a new finality certificate from the given power delta (from
// `MakePowerTableDiff`) and justification (from GPBFT).
//
// Note, however, that this function does not attempt to validate the resulting finality
// certificate (beyond verifying that it is a justification for the correct round). You can do so by
// immediately calling `ValidateFinalityCertificates` on the result.
func NewFinalityCertificate(powerDelta []PowerTableDelta, justification *gpbft.Justification) (*FinalityCertificate, error) {
	if justification.Vote.Step != gpbft.DECIDE_PHASE {
		return nil, xerrors.Errorf("can only create a finality certificate from a decide vote, got phase %s", justification.Vote.Step)
	}

	if justification.Vote.Round != 0 {
		return nil, xerrors.Errorf("expected decide round to be 0, got round %d", justification.Vote.Round)
	}

	if justification.Vote.Value.IsZero() {
		return nil, xerrors.Errorf("got a decision for bottom for instance %d", justification.Vote.Instance)
	}

	return &FinalityCertificate{
		GPBFTInstance:    justification.Vote.Instance,
		SupplementalData: justification.Vote.SupplementalData,
		ECChain:          justification.Vote.Value,
		Signers:          justification.Signers,
		Signature:        justification.Signature,
		PowerTableDelta:  powerDelta,
	}, nil
}

// ValidateFinalityCertificates validates zero or more finality certificates, returning the next
// instance after the last valid finality certificates, any newly finalized tipsets, and the next
// power table to use as-of the last valid finality certificate. If passed a non-nil `base` tipset,
// validate that the finalized chain starts with that tipset (accept any finalized chain otherwise).
//
// Returns an error if it encounters any invalid finality certificates, along with the last valid
// instance, finalized chain epochs, etc. E.g., if provided a partially valid chain of finality
// certificates, this function will return a (possibly empty) prefix of the EC chain correctly
// finalized, the instance of the first invalid finality certificate, and the power table that
// should be used to validate that finality certificate, along with the error encountered.
func ValidateFinalityCertificates(verifier gpbft.Verifier, network gpbft.NetworkName, prevPowerTable gpbft.PowerEntries, nextInstance uint64, base *gpbft.TipSet,
	certs ...FinalityCertificate) (_nextInstance uint64, chain gpbft.ECChain, newPowerTable gpbft.PowerEntries, err error) {
	for _, cert := range certs {
		if cert.GPBFTInstance != nextInstance {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("expected instance %d, found instance %d", nextInstance, cert.GPBFTInstance)
		}
		// Some basic sanity checks.
		if err := cert.ECChain.Validate(); err != nil {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("invalid finality certificate at instance %d: %w", cert.GPBFTInstance, err)
		}

		// We can't have a finality certificate for "bottom"
		if cert.ECChain.IsZero() {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("empty finality certificate for instance %d", cert.GPBFTInstance)
		}

		// Validate that the base is as expected if specified. Otherwise, skip this check
		// for the first finality certificate.
		if base != nil && !base.Equal(cert.ECChain.Base()) {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("base tipset does not match finalized chain at instance %d", cert.GPBFTInstance)
		}

		// Validate signature.
		if err := verifyFinalityCertificateSignature(verifier, prevPowerTable, network, cert); err != nil {
			return nextInstance, chain, prevPowerTable, err
		}

		// Now compute the new power table and validate that it matches the power table for
		// new head.
		newPowerTable, err = ApplyPowerTableDiff(prevPowerTable, cert.PowerTableDelta)
		if err != nil {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("failed to apply power table delta for finality certificate for instance %d: %w", cert.GPBFTInstance, err)
		}

		powerTableCid, err := MakePowerTableCID(newPowerTable)
		if err != nil {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf("failed to make power table CID for finality certificate for instance %d: %w", cert.GPBFTInstance, err)
		}

		if !bytes.Equal(cert.SupplementalData.PowerTable, powerTableCid) {
			return nextInstance, chain, prevPowerTable, xerrors.Errorf(
				"incorrect power diff from finality certificate for instance %d: expected %+v, got %+v",
				cert.GPBFTInstance, cert.SupplementalData.PowerTable, powerTableCid)
		}
		nextInstance++
		chain = append(chain, cert.ECChain.Suffix()...)
		prevPowerTable = newPowerTable
		base = cert.ECChain.Head()
	}

	return nextInstance, chain, newPowerTable, nil
}

// Verify the signature of the given finality certificate. This doesn't validate the power delta, or
// any other parts of the certificate, just that the _value_ has been signed by a majority of the
// power.
func verifyFinalityCertificateSignature(verifier gpbft.Verifier, powerTable gpbft.PowerEntries, nn gpbft.NetworkName, cert FinalityCertificate) error {
	totalPower := new(gpbft.StoragePower)
	for i := range powerTable {
		totalPower = totalPower.Add(totalPower, powerTable[i].Power)
	}

	signers := make([]gpbft.PubKey, 0, len(powerTable))
	signerPower := new(gpbft.StoragePower)
	if err := cert.Signers.ForEach(func(i uint64) error {
		if i >= uint64(len(powerTable)) {
			return xerrors.Errorf(
				"finality certificate for instance %d specifies a signer %d but we only have %d entries in the power table",
				cert.GPBFTInstance, i, len(powerTable))
		}
		signer := &powerTable[i]
		signerPower = signerPower.Add(signerPower, signer.Power)
		signers = append(signers, signer.PubKey)
		return nil
	}); err != nil {
		return err
	}

	if !gpbft.IsStrongQuorum(signerPower, totalPower) {
		return xerrors.Errorf("finality certificate for instance %d has insufficient power: %s < 2/3 %s", cert.GPBFTInstance, signerPower, totalPower)
	}

	payload := &gpbft.Payload{
		Instance:         cert.GPBFTInstance,
		Round:            0,
		SupplementalData: cert.SupplementalData,
		Step:             gpbft.DECIDE_PHASE,
		Value:            cert.ECChain,
	}

	// We use SigningMarshaler when implemented (for testing), but only require a `Verifier` in
	// the function signature to make it easier to use this as a free function.
	var signedBytes []byte
	if sig, ok := verifier.(gpbft.SigningMarshaler); ok {
		signedBytes = sig.MarshalPayloadForSigning(nn, payload)
	} else {
		signedBytes = payload.MarshalForSigning(nn)
	}

	if err := verifier.VerifyAggregate(signedBytes, cert.Signature, signers); err != nil {
		return xerrors.Errorf("invalid signature on finality certificate for instance %d: %w", cert.GPBFTInstance, err)
	}
	return nil
}

// MakePowerTableDiff create a power table diff between the two given power tables. It makes no
// assumptions about order, but does assume that the power table entries are unique. The returned
// diff is sorted by participant ID ascending.
func MakePowerTableDiff(oldPowerTable, newPowerTable gpbft.PowerEntries) []PowerTableDelta {
	oldPowerMap := make(map[gpbft.ActorID]*gpbft.PowerEntry, len(oldPowerTable))
	for i := range oldPowerTable {
		e := &oldPowerTable[i]
		oldPowerMap[e.ID] = e
	}

	var diff []PowerTableDelta
	for i := range newPowerTable {
		newEntry := &newPowerTable[i]
		delta := PowerTableDelta{ParticipantID: newEntry.ID}
		if oldEntry, ok := oldPowerMap[newEntry.ID]; ok {
			delete(oldPowerMap, newEntry.ID)
			delta.PowerDelta = new(gpbft.StoragePower).Sub(newEntry.Power, oldEntry.Power)
			if !bytes.Equal(newEntry.PubKey, oldEntry.PubKey) {
				delta.SigningKey = newEntry.PubKey
			}
			if delta.IsZero() {
				continue
			}
		} else {
			delta.PowerDelta = newEntry.Power
			delta.SigningKey = newEntry.PubKey
		}
		diff = append(diff, delta)
	}
	for _, e := range oldPowerMap {
		diff = append(diff, PowerTableDelta{
			ParticipantID: e.ID,
			PowerDelta:    new(gpbft.StoragePower).Neg(e.Power),
		})
	}
	slices.SortFunc(diff, func(a, b PowerTableDelta) int {
		return cmp.Compare(a.ParticipantID, b.ParticipantID)
	})
	return diff
}

// Apply a power table diff to the passed power table.
//
// - The delta must be sorted by participant ID, ascending.
// - The returned power table is sorted by power, descending.
func ApplyPowerTableDiff(prevPowerTable gpbft.PowerEntries, delta []PowerTableDelta) (gpbft.PowerEntries, error) {
	deltaMap := make(map[gpbft.ActorID]*PowerTableDelta, len(delta))
	var lastActorId gpbft.ActorID
	for i := range delta {
		d := &delta[i]

		// We assert this to make sure the finality certificate has a consistent power-table
		// diff.
		if i > 0 && d.ParticipantID <= lastActorId {
			return nil, xerrors.Errorf("power table delta not sorted by participant ID")
		}

		deltaMap[d.ParticipantID] = d
		lastActorId = d.ParticipantID
	}

	// NOTE: we don't check if the new power is negative or anything like that because
	// we're just going to serialize to CBOR and validate that the hashes map. So the
	// only sanity check we need is the above "sort" check.
	newPowerTable := make(gpbft.PowerEntries, 0, len(prevPowerTable)+len(deltaMap))
	for _, pe := range prevPowerTable {
		if diff, ok := deltaMap[pe.ID]; ok {
			delete(deltaMap, pe.ID)

			if diff.PowerDelta.Sign() != 0 {
				pe.Power = new(gpbft.StoragePower).Add(diff.PowerDelta, pe.Power)
				if pe.Power.Sign() == 0 {
					continue
				}
			}
			if len(diff.SigningKey) > 0 {
				pe.PubKey = diff.SigningKey
			}
		}
		newPowerTable = append(newPowerTable, pe)
	}

	for _, diff := range deltaMap {
		newPowerTable = append(newPowerTable, gpbft.PowerEntry{
			ID:     diff.ParticipantID,
			Power:  diff.PowerDelta,
			PubKey: diff.SigningKey,
		})
	}

	sort.Sort(newPowerTable)
	return newPowerTable, nil
}

// MakePowerTableCID returns the DagCBOR-blake2b256 CID of the given power entries. This method does
// not mutate, sort, validate, etc. the power entries.
func MakePowerTableCID(pt gpbft.PowerEntries) (gpbft.CID, error) {
	var buf bytes.Buffer
	if err := pt.MarshalCBOR(&buf); err != nil {
		return nil, xerrors.Errorf("failed to serialize power table: %w", err)
	}
	return gpbft.MakeCid(buf.Bytes()), nil
}
