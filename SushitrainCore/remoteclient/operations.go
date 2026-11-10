// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// EncodeOperation is the body an operation is submitted with. A client stores it before sending it the first time,
// and sends exactly these bytes on every retry (invariant 7).
func EncodeOperation(op remoteapi.OperationRequest) ([]byte, error) {
	return json.Marshal(op)
}

// SubmitOperation submits an encoded operation. A replay of the same body returns the stored result. The result's
// State is committed, conflict or pending (poll it with Operation); a failure is an Error, with the current item for
// a 412 or name conflict, and the operation when the gateway recorded one.
func (c *Client) SubmitOperation(ctx context.Context, body []byte) (remoteapi.Operation, error) {
	var op remoteapi.Operation
	_, err := c.call(ctx, "POST", []string{"v1", "operations"}, nil, withCredential, body, &op, KindOther, 0)
	return op, err
}

// Operation returns the state of a submitted operation by its gateway ID (op_…). KindNotFound means the gateway
// doesn't know it (any more).
func (c *Client) Operation(ctx context.Context, operationID string) (remoteapi.Operation, error) {
	var op remoteapi.Operation
	_, err := c.call(ctx, "GET", []string{"v1", "operations", operationID}, nil, withCredential, nil, &op, KindOther, 0)
	return op, err
}

// RequestPairing submits the secret of a pairing code, without any credential
func (c *Client) RequestPairing(ctx context.Context, req remoteapi.PairingRequest) (remoteapi.PairingPending, error) {
	var pending remoteapi.PairingPending
	_, err := c.call(ctx, "POST", []string{"v1", "pairing"}, nil, withNothing, req, &pending, KindOther, 0)
	return pending, err
}

// PollPairing asks whether a pairing request was approved, waiting up to wait for an answer. The credential comes
// back when it was; retries recover the same credential until expiry, revocation or re-pairing. A denial is
// KindForbidden (pairing_denied), an expired request a 410 (pairing_expired).
func (c *Client) PollPairing(ctx context.Context, requestID, pollToken string, wait time.Duration) (
	remoteapi.PairingResult, error) {
	query := url.Values{}
	if seconds := int(wait / time.Second); seconds > 0 {
		query.Set("wait", strconv.Itoa(seconds))
	}
	var result remoteapi.PairingResult
	_, err := c.call(ctx, "GET", []string{"v1", "pairing", requestID}, query, withToken(pollToken), nil, &result,
		KindOther, wait)
	return result, err
}

// ErrPairingDenied and ErrPairingExpired end a pairing that won't succeed
var (
	ErrPairingDenied  = errors.New("the gateway denied pairing")
	ErrPairingExpired = errors.New("the pairing code was used or has expired")
)

// Pairing is a client asking a gateway to pair with it
type Pairing struct {
	ClientID   string
	ClientName string
	Platform   string
	// Called once the gateway has the request, while it waits for approval on the gateway
	Requested func(remoteapi.PairingPending)
}

// Pair pairs with the gateway an invitation is from: it submits the secret, then waits until the request is approved
// on the gateway and returns the credential. The gateway must be the one the invitation names.
func Pair(ctx context.Context, inv Invitation, p Pairing) (string, error) {
	client := New(inv.Endpoint(), func() (string, error) { return "", nil })
	defer client.Close()
	pending, err := client.RequestPairing(ctx, remoteapi.PairingRequest{Secret: inv.Secret, ServiceID: inv.ServiceID,
		ClientID: p.ClientID, ClientName: p.ClientName, Platform: p.Platform})
	if err != nil {
		return "", pairingError(err)
	}
	if pending.ServiceID != inv.ServiceID {
		return "", protocolError("the gateway is another service than the pairing code's")
	}
	if p.Requested != nil {
		p.Requested(pending)
	}
	for {
		result, err := client.PollPairing(ctx, pending.Request, pending.PollToken, 25*time.Second)
		if err != nil {
			if IsTransient(err) && ctx.Err() == nil {
				// The gateway may restart or be briefly out of reach while someone approves the request
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
				continue
			}
			return "", pairingError(err)
		}
		switch {
		case result.State == remoteapi.PairingStateApproved && result.Credential != "":
			if result.ServiceID != "" && result.ServiceID != inv.ServiceID {
				return "", protocolError("the credential is from another service than the pairing code's")
			}
			return result.Credential, nil
		case result.State == remoteapi.PairingStatePending:
			if err := ctx.Err(); err != nil {
				return "", &Error{Kind: KindCanceled, Message: "canceled", Err: err}
			}
		default:
			return "", protocolError("unexpected pairing state %q", result.State)
		}
	}
}

func pairingError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		switch {
		case e.Code == remoteapi.CodePairingDenied:
			return errors.Join(ErrPairingDenied, err)
		case e.Code == remoteapi.CodePairingExpired, e.Status == http.StatusGone:
			return errors.Join(ErrPairingExpired, err)
		}
	}
	return err
}
