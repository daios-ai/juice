// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"crypto/ed25519"
	"strings"
)

// A trace-scoped capability lets a dispatched kind=http action compose within its own call
// (§9): it is the call's trace_id signed with the platform key, valid only while the trace is
// unsettled. It carries no authority of its own — verification yields the trace and its action
// owner, and composition then runs under the ordinary role law (§4/§6) as that owner. The token
// is a header credential, never serialized into args, replies, receipts, or logs (R9).
//
// Format: "<trace_id>.<sigB64>", where sig is the Ed25519 signature over the JCS-canonical
// object {"cap": trace_id}. That object's key set is disjoint from receipts, manifests, and
// federation payloads, keeping the capability in its own signature domain (§12).

func capPayload(traceID string) map[string]string { return map[string]string{"cap": traceID} }

// IssueCapability mints a capability for a live trace. Requires a configured signing key.
func (k *Kernel) IssueCapability(traceID string) (string, error) {
	sig, err := k.cfg.Network.sign(k.cfg.SigningKey, sigDomainCapability, capPayload(traceID))
	if err != nil {
		return "", err
	}
	return traceID + "." + sig, nil
}

// VerifyCapability validates a capability token and returns the trace it names and that trace's
// action owner (the identity composition runs as). It rejects a malformed or tampered token, a
// token for a missing trace, and a token whose trace has settled (a transaction exists for it) —
// settlement thereby invalidates every capability for the trace with no separate revocation.
func (k *Kernel) VerifyCapability(ctx context.Context, token string) (traceID, ownerID string, err error) {
	dot := strings.LastIndex(token, ".")
	if dot <= 0 || dot == len(token)-1 {
		return "", "", ErrUnauthorized.Wrap("malformed capability")
	}
	traceID, sig := token[:dot], token[dot+1:]

	pub, ok := k.cfg.SigningKey.Public().(ed25519.PublicKey)
	if !ok {
		return "", "", ErrInvalidState.Wrap("signing key is not configured")
	}
	if err := k.cfg.Network.verify(pub, sigDomainCapability, capPayload(traceID), sig); err != nil {
		return "", "", ErrUnauthorized.Wrap("invalid capability")
	}

	trace, err := k.store.ReadTrace(ctx, traceID)
	if err != nil || trace == nil {
		return "", "", ErrUnauthorized.Wrap("capability trace not found")
	}
	// An early, readable refusal. The rule itself is enforced where it cannot go stale: the funding
	// statement refuses a settled trace or a closed process in the same predicate as the funds (§9).
	settled, err := k.store.TraceHasTransaction(ctx, traceID)
	if err != nil {
		return "", "", err
	}
	if settled {
		return "", "", ErrUnauthorized.Wrap("capability expired: call already settled")
	}
	return traceID, trace.ActionOwnerID, nil
}
