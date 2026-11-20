// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.ate.dev/substrate/api/credprovider/v1"
)

// fakeAccessor is a fake Secret Manager client serving payloads by resource
// name.
type fakeAccessor struct {
	byName map[string][]byte
	// errByName maps a resource name to an error to return instead.
	errByName map[string]error
	// corrupt sends a CRC32C that does not match the data.
	corrupt bool
	// noPayload omits the payload.
	noPayload bool
	// noCRC omits the CRC32C.
	noCRC bool
	// block answers only once the call's context is done.
	block bool
	// gate, if set, holds each call until it is closed or the call's context
	// is done.
	gate chan struct{}
	// gotName is the last requested resource name. Calls may overlap, so they
	// set it under mu; read it once they are done.
	gotName string
	mu      sync.Mutex
	// calls counts the calls made.
	calls atomic.Int32
}

func (f *fakeAccessor) AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.gotName = req.GetName()
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if f.block {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err, ok := f.errByName[req.GetName()]; ok {
		return nil, err
	}
	data, ok := f.byName[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no such secret version")
	}
	if f.noPayload {
		return &secretmanagerpb.AccessSecretVersionResponse{Name: req.GetName()}, nil
	}
	if f.noCRC {
		return &secretmanagerpb.AccessSecretVersionResponse{
			Name:    req.GetName(),
			Payload: &secretmanagerpb.SecretPayload{Data: data},
		}, nil
	}
	crc := int64(crc32.Checksum(data, crc32cTable))
	if f.corrupt {
		crc++ // deliberately wrong
	}
	return &secretmanagerpb.AccessSecretVersionResponse{
		Name:    req.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: data, DataCrc32C: &crc},
	}, nil
}

// only serves global secrets through a and fails for any region.
func only(a SecretAccessor) Accessors {
	return func(location string) (SecretAccessor, error) {
		if location != "" {
			return nil, fmt.Errorf("no client for %q", location)
		}
		return a, nil
	}
}

func TestParseURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		want    secretRef
		wantErr bool
	}{
		{
			name: "with version",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/5",
			want: secretRef{Project: "proj-123", Secret: "example-api", Version: "5"},
		},
		{
			name: "latest alias",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/latest",
			want: secretRef{Project: "proj-123", Secret: "example-api", Version: "latest"},
		},
		{
			name: "with key",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/5/keys/api_key",
			want: secretRef{Project: "proj-123", Secret: "example-api", Version: "5", Key: "api_key"},
		},
		{
			name: "latest alias with key",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/latest/keys/token",
			want: secretRef{Project: "proj-123", Secret: "example-api", Version: "latest", Key: "token"},
		},
		// The version is never implied; "latest" must be spelled out.
		{name: "version required", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api", wantErr: true},
		{name: "version required with key", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/keys/token", wantErr: true},
		{name: "wrong scheme", uri: "https://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "wrong provider", uri: "ate-secret://k8s.io/projects/proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "too few segments", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets", wantErr: true},
		{name: "wrong projects keyword", uri: "ate-secret://secretmanager.googleapis.com/project/proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "wrong secrets keyword", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secret/example-api/versions/1", wantErr: true},
		{name: "wrong versions keyword", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/version/5", wantErr: true},
		{name: "non-numeric version", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/newest", wantErr: true},
		{name: "zero version", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/0", wantErr: true},
		{name: "leading-zero version", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/05", wantErr: true},
		{name: "singular key keyword", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/s/versions/1/key/token", wantErr: true},
		{name: "keys keyword without a key", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/s/versions/1/keys", wantErr: true},
		{name: "too many segments", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/s/versions/1/keys/token/extra", wantErr: true},
		{name: "empty segment", uri: "ate-secret://secretmanager.googleapis.com/projects//secrets/example-api/versions/1", wantErr: true},
		{name: "query not allowed", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1?key=token", wantErr: true},
		{name: "fragment not allowed", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1#x", wantErr: true},
		{name: "percent-encoded separator", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example%2Fapi/versions/1", wantErr: true},
		{name: "percent-encoding of any kind", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example%2Dapi/versions/1", wantErr: true},
		{name: "percent-encoding in key", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1/keys/api%2Dkey", wantErr: true},
		{name: "space in path", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example api/versions/1", wantErr: true},
		{name: "unparseable", uri: "://://", wantErr: true},
		{
			name: "project number",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/123456789012/secrets/example-api/versions/1",
			want: secretRef{Project: "123456789012", Secret: "example-api", Version: "1"},
		},
		{name: "user information", uri: "ate-secret://user@secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "doubled slash", uri: "ate-secret://secretmanager.googleapis.com/projects//proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "leading doubled slash", uri: "ate-secret://secretmanager.googleapis.com//projects/proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "trailing slash", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1/", wantErr: true},
		{name: "no path", uri: "ate-secret://secretmanager.googleapis.com", wantErr: true},
		{name: "project with a character Secret Manager rejects", uri: "ate-secret://secretmanager.googleapis.com/projects/proj!123/secrets/example-api/versions/1", wantErr: true},
		{name: "project in upper case", uri: "ate-secret://secretmanager.googleapis.com/projects/Proj-123/secrets/example-api/versions/1", wantErr: true},
		{name: "project too short", uri: "ate-secret://secretmanager.googleapis.com/projects/p/secrets/example-api/versions/1", wantErr: true},
		{name: "project as a dot segment", uri: "ate-secret://secretmanager.googleapis.com/projects/../secrets/example-api/versions/1", wantErr: true},
		{name: "secret with a dot", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example.api/versions/1", wantErr: true},
		{name: "secret as a dot segment", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/../versions/1", wantErr: true},
		{name: "secret too long", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/" + strings.Repeat("s", 256) + "/versions/1", wantErr: true},
		{
			name: "regional",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/us-central1/secrets/example-api/versions/5",
			want: secretRef{Project: "proj-123", Location: "us-central1", Secret: "example-api", Version: "5"},
		},
		{
			name: "regional with key",
			uri:  "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/europe-west4/secrets/example-api/versions/latest/keys/token",
			want: secretRef{Project: "proj-123", Location: "europe-west4", Secret: "example-api", Version: "latest", Key: "token"},
		},
		{name: "regional without a version", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/us-central1/secrets/example-api", wantErr: true},
		{name: "locations keyword without a location", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/secrets/example-api/versions/1", wantErr: true},
		{name: "locations keyword with nothing after", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/us-central1", wantErr: true},
		// The location becomes part of an endpoint hostname.
		{name: "location with a dot", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/example.com/secrets/example-api/versions/1", wantErr: true},
		{name: "location in upper case", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/US-CENTRAL1/secrets/example-api/versions/1", wantErr: true},
		{name: "location global", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/global/secrets/example-api/versions/1", wantErr: true},
		{name: "regional with too many segments", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/us-central1/secrets/example-api/versions/1/keys/token/extra", wantErr: true},
		{name: "key as a dot segment", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1/keys/..", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseURI(%q) = %+v, want error", tc.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseURI(%q) unexpected error: %v", tc.uri, err)
			}
			if got != tc.want {
				t.Errorf("parseURI(%q) = %+v, want %+v", tc.uri, got, tc.want)
			}
		})
	}
}

func TestResourceName(t *testing.T) {
	for _, tc := range []struct {
		ref  secretRef
		want string
	}{
		{secretRef{Project: "proj-123", Secret: "example-api", Version: "latest"}, "projects/proj-123/secrets/example-api/versions/latest"},
		{secretRef{Project: "proj-123", Location: "us-central1", Secret: "example-api", Version: "3"}, "projects/proj-123/locations/us-central1/secrets/example-api/versions/3"},
	} {
		if got := tc.ref.resourceName(); got != tc.want {
			t.Errorf("%+v.resourceName() = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// testActor is an actor in the atespace testAuthorizer grants proj-123.
const testActor = "spiffe://substrate-actor.local/actor/team-a/agent-1"

func testAuthorizer(t *testing.T) *ProjectAuthorizer {
	t.Helper()
	a, err := newProjectAuthorizer(projectPolicyFile{Policies: []atespaceProjectPolicy{
		{Atespace: "team-a", AllowedProjects: []string{"proj-123"}},
	}})
	if err != nil {
		t.Fatalf("newProjectAuthorizer: %v", err)
	}
	return a
}

func TestFetchSecret(t *testing.T) {
	const secret = "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api"
	const uri = secret + "/versions/latest"
	const name = "projects/proj-123/secrets/example-api/versions/latest"
	withPayload := func(payload string) *fakeAccessor {
		return &fakeAccessor{byName: map[string][]byte{name: []byte(payload)}}
	}
	tests := []struct {
		name     string
		accessor *fakeAccessor
		uri      string
		want     string
		wantCode codes.Code
	}{
		{
			// Returned verbatim; the gateway trims and prefixes it.
			name:     "found",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t\n")}},
			uri:      uri,
			want:     "s3cr3t\n",
		},
		{
			name:     "no checksum",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}, noCRC: true},
			uri:      uri,
			want:     "s3cr3t",
		},
		{
			// Only the selected value is decoded.
			name:     "key beside a value Go cannot represent",
			accessor: withPayload(`{"api_key":"s3cr3t","n":1e999}`),
			uri:      uri + "/keys/api_key",
			want:     "s3cr3t",
		},
		{
			name:     "key whose value Go cannot represent",
			accessor: withPayload(`{"api_key":"s3cr3t","n":1e999}`),
			uri:      uri + "/keys/n",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "numbered version",
			accessor: &fakeAccessor{byName: map[string][]byte{"projects/proj-123/secrets/example-api/versions/5": []byte("v5")}},
			uri:      secret + "/versions/5",
			want:     "v5",
		},
		{
			// Refused rather than read as "latest".
			name:     "version required",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}},
			uri:      secret,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "json payload without key",
			accessor: withPayload(`{"api_key":"s3cr3t"}`),
			uri:      uri,
			want:     `{"api_key":"s3cr3t"}`,
		},
		{
			name:     "key",
			accessor: withPayload(`{"api_key":"s3cr3t","other":"x"}`),
			uri:      uri + "/keys/api_key",
			want:     "s3cr3t",
		},
		{
			name:     "key value is unescaped",
			accessor: withPayload(`{"token":"a\"b\n"}`),
			uri:      uri + "/keys/token",
			want:     "a\"b\n",
		},
		{
			name:     "key missing is not found",
			accessor: withPayload(`{"api_key":"s3cr3t"}`),
			uri:      uri + "/keys/token",
			wantCode: codes.NotFound,
		},
		{
			name:     "key on non-json payload",
			accessor: withPayload("s3cr3t"),
			uri:      uri + "/keys/token",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "key on json array",
			accessor: withPayload(`["s3cr3t"]`),
			uri:      uri + "/keys/token",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "key on json null",
			accessor: withPayload("null"),
			uri:      uri + "/keys/token",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "key value not a string",
			accessor: withPayload(`{"token":12345}`),
			uri:      uri + "/keys/token",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "key value null",
			accessor: withPayload(`{"token":null}`),
			uri:      uri + "/keys/token",
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "key in nested object is not reached",
			accessor: withPayload(`{"outer":{"token":"s3cr3t"}}`),
			uri:      uri + "/keys/token",
			wantCode: codes.NotFound,
		},
		{
			name:     "crc mismatch fails closed before key lookup",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte(`{"token":"s3cr3t"}`)}, corrupt: true},
			uri:      uri + "/keys/token",
			wantCode: codes.Unavailable,
		},
		{
			name:     "not found",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.NotFound, "gone")}},
			uri:      uri,
			wantCode: codes.NotFound,
		},
		{
			name:     "permission denied maps through",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.PermissionDenied, "nope")}},
			uri:      uri,
			wantCode: codes.PermissionDenied,
		},
		// Only Unavailable and DeadlineExceeded are retryable at the gateway, so
		// failures a retry cannot fix keep another code.
		{
			// A disabled or destroyed version, e.g. "latest" while the newest is disabled.
			name:     "failed precondition passes through",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.FailedPrecondition, "version is disabled")}},
			uri:      uri,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "invalid argument passes through",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.InvalidArgument, "bad name")}},
			uri:      uri,
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "deadline exceeded passes through",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.DeadlineExceeded, "slow")}},
			uri:      uri,
			wantCode: codes.DeadlineExceeded,
		},
		{
			// The provider's own credentials refused, e.g. misconfigured Workload Identity.
			name:     "unauthenticated is permission denied",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.Unauthenticated, "no credentials")}},
			uri:      uri,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "internal error is unavailable",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.Internal, "boom")}},
			uri:      uri,
			wantCode: codes.Unavailable,
		},
		{
			name:     "quota exhaustion is unavailable",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.ResourceExhausted, "quota")}},
			uri:      uri,
			wantCode: codes.Unavailable,
		},
		{
			name:     "unavailable stays unavailable",
			accessor: &fakeAccessor{errByName: map[string]error{name: status.Error(codes.Unavailable, "down")}},
			uri:      uri,
			wantCode: codes.Unavailable,
		},
		{
			// Not a gRPC status, e.g. a transport failure.
			name:     "unknown error is unavailable",
			accessor: &fakeAccessor{errByName: map[string]error{name: errors.New("connection reset")}},
			uri:      uri,
			wantCode: codes.Unavailable,
		},
		{
			name:     "no payload is not found",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}, noPayload: true},
			uri:      uri,
			wantCode: codes.NotFound,
		},
		{
			name:     "crc mismatch fails closed",
			accessor: &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}, corrupt: true},
			uri:      uri,
			wantCode: codes.Unavailable,
		},
		{
			name:     "bad uri",
			accessor: &fakeAccessor{},
			uri:      "ate-secret://k8s.io/projects/proj-123/secrets/example-api",
			wantCode: codes.InvalidArgument,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(only(tc.accessor), testAuthorizer(t), time.Second, 0)
			resp, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{Uri: tc.uri, ActorSpiffeId: testActor})
			if tc.wantCode != codes.OK {
				if status.Code(err) != tc.wantCode {
					t.Fatalf("FetchSecret(%q) code = %v, want %v (err=%v)", tc.uri, status.Code(err), tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchSecret(%q) unexpected error: %v", tc.uri, err)
			}
			if got := string(resp.GetOpaqueBytes()); got != tc.want {
				t.Errorf("FetchSecret(%q) = %q, want %q", tc.uri, got, tc.want)
			}
		})
	}
}

// A decode error omits the decoder's message, which quotes the secret.
func TestFetchSecretKeyErrorDoesNotQuotePayload(t *testing.T) {
	const name = "projects/proj-123/secrets/example-api/versions/1"
	accessor := &fakeAccessor{byName: map[string][]byte{name: []byte(`{"token": Xs3cr3t}`)}}
	_, err := NewServer(only(accessor), testAuthorizer(t), time.Second, 0).FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
		Uri:           "ate-secret://secretmanager.googleapis.com/" + name + "/keys/token",
		ActorSpiffeId: testActor,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "X") {
		t.Errorf("error %q quotes the payload", msg)
	}
}

// A URI that fails to parse is refused before Secret Manager is dialed.
func TestFetchSecretDoesNotDialOnBadURI(t *testing.T) {
	accessor := &fakeAccessor{}
	_, err := NewServer(only(accessor), testAuthorizer(t), time.Second, 0).FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
		Uri:           "ate-secret://secretmanager.googleapis.com/nope",
		ActorSpiffeId: testActor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if accessor.gotName != "" {
		t.Errorf("Secret Manager was asked for %q; a bad URI must not be dialed", accessor.gotName)
	}
}

// Only projects granted to the actor's atespace resolve, and a refused request
// never reaches Secret Manager.
func TestFetchSecretAuthorization(t *testing.T) {
	const name = "projects/proj-123/secrets/example-api/versions/1"
	const uri = "ate-secret://secretmanager.googleapis.com/" + name
	authz, err := newProjectAuthorizer(projectPolicyFile{Policies: []atespaceProjectPolicy{
		{Atespace: "team-a", AllowedProjects: []string{"proj-123"}},
		{Atespace: "team-b", AllowedProjects: []string{"proj-456"}},
	}})
	if err != nil {
		t.Fatalf("newProjectAuthorizer: %v", err)
	}

	for _, tc := range []struct {
		name     string
		authz    *ProjectAuthorizer
		actor    string
		wantCode codes.Code
	}{
		{name: "project granted", authz: authz, actor: "spiffe://substrate-actor.local/actor/team-a/agent-1"},
		{name: "another project granted", authz: authz, actor: "spiffe://substrate-actor.local/actor/team-b/agent-1", wantCode: codes.PermissionDenied},
		{name: "atespace absent from the policy", authz: authz, actor: "spiffe://substrate-actor.local/actor/team-c/agent-1", wantCode: codes.PermissionDenied},
		{name: "no actor", authz: authz, actor: "", wantCode: codes.PermissionDenied},
		{name: "not an actor SPIFFE ID", authz: authz, actor: "spiffe://cluster.local/ns/team-a/sa/default", wantCode: codes.PermissionDenied},
		{name: "no policy", authz: nil, actor: "spiffe://substrate-actor.local/actor/team-a/agent-1", wantCode: codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accessor := &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}}
			resp, err := NewServer(only(accessor), tc.authz, time.Second, 0).FetchSecret(context.Background(), &credprovider.FetchSecretRequest{Uri: uri, ActorSpiffeId: tc.actor})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", status.Code(err), tc.wantCode, err)
			}
			if tc.wantCode == codes.OK {
				if got := string(resp.GetOpaqueBytes()); got != "s3cr3t" {
					t.Errorf("FetchSecret = %q, want s3cr3t", got)
				}
				return
			}
			if accessor.gotName != "" {
				t.Errorf("Secret Manager was asked for %q; a refused request must not reach it", accessor.gotName)
			}
		})
	}
}

// A Secret Manager call that outlasts the fetch timeout fails with
// DeadlineExceeded, which the gateway retries.
func TestFetchSecretTimesOut(t *testing.T) {
	const timeout = 50 * time.Millisecond
	accessor := &fakeAccessor{block: true}
	start := time.Now()
	_, err := NewServer(only(accessor), testAuthorizer(t), timeout, 0).FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
		Uri:           "ate-secret://secretmanager.googleapis.com/projects/proj-123/secrets/example-api/versions/1",
		ActorSpiffeId: testActor,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("code = %v, want DeadlineExceeded (err=%v)", status.Code(err), err)
	}
	if elapsed := time.Since(start); elapsed < timeout || elapsed > 10*timeout {
		t.Errorf("FetchSecret returned after %v, want about %v", elapsed, timeout)
	}
}

// Fetches of one secret version within the TTL share a single read, with or
// without a key. Authorization still runs on a cached payload, a failed read is
// not cached, and an expired entry is read again.
func TestFetchSecretCache(t *testing.T) {
	const name = "projects/proj-123/secrets/example-api/versions/latest"
	const uri = "ate-secret://secretmanager.googleapis.com/" + name
	// team-a may resolve secrets in proj-123; team-b may not.
	authz, err := newProjectAuthorizer(projectPolicyFile{Policies: []atespaceProjectPolicy{
		{Atespace: "team-a", AllowedProjects: []string{"proj-123"}},
		{Atespace: "team-b", AllowedProjects: []string{"proj-456"}},
	}})
	if err != nil {
		t.Fatalf("newProjectAuthorizer: %v", err)
	}
	const teamA = "spiffe://substrate-actor.local/actor/team-a/agent-1"
	const teamB = "spiffe://substrate-actor.local/actor/team-b/agent-1"
	const v1 = `{"token":"s3cr3t","user":"bob"}`
	const v2 = `{"token":"r0tated","user":"bob"}`

	type fetch struct {
		// wait is how long to sleep before this fetch, on synctest's clock.
		wait time.Duration
		// store, if set, becomes the secret's payload before this fetch. Until
		// a fetch stores one, the secret does not exist.
		store string
		actor string
		// key, if set, is the JSON key to fetch instead of the whole payload.
		key      string
		want     string
		wantCode codes.Code
	}
	tests := []struct {
		name      string
		ttl       time.Duration
		fetches   []fetch
		wantReads int32
	}{
		{
			name: "keys and the whole payload share a read",
			ttl:  time.Hour,
			fetches: []fetch{
				{store: v1, actor: teamA, key: "token", want: "s3cr3t"},
				{actor: teamA, key: "user", want: "bob"},
				{actor: teamA, want: v1},
			},
			wantReads: 1,
		},
		{
			name: "zero ttl reads every time",
			fetches: []fetch{
				{store: v1, actor: teamA, key: "token", want: "s3cr3t"},
				{actor: teamA, key: "user", want: "bob"},
			},
			wantReads: 2,
		},
		{
			name: "cached payload still checked against the project policy",
			ttl:  time.Hour,
			fetches: []fetch{
				{store: v1, actor: teamA, key: "token", want: "s3cr3t"},
				{actor: teamB, key: "token", wantCode: codes.PermissionDenied},
			},
			wantReads: 1,
		},
		{
			name: "failed read not cached",
			ttl:  time.Hour,
			fetches: []fetch{
				{actor: teamA, key: "token", wantCode: codes.NotFound},
				{store: v1, actor: teamA, key: "token", want: "s3cr3t"},
			},
			wantReads: 2,
		},
		{
			name: "new payload served once the entry expires",
			ttl:  10 * time.Second,
			fetches: []fetch{
				{store: v1, actor: teamA, key: "token", want: "s3cr3t"},
				{wait: 9 * time.Second, store: v2, actor: teamA, key: "token", want: "s3cr3t"},
				{wait: time.Second, actor: teamA, key: "token", want: "r0tated"},
			},
			wantReads: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				accessor := &fakeAccessor{byName: map[string][]byte{}}
				srv := NewServer(only(accessor), authz, time.Second, tc.ttl)
				for i, f := range tc.fetches {
					if f.store != "" {
						accessor.byName[name] = []byte(f.store)
					}
					time.Sleep(f.wait)
					fetchURI := uri
					if f.key != "" {
						fetchURI += "/keys/" + f.key
					}
					resp, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{Uri: fetchURI, ActorSpiffeId: f.actor})
					if status.Code(err) != f.wantCode {
						t.Fatalf("fetch %d: code = %v, want %v (err=%v)", i, status.Code(err), f.wantCode, err)
					}
					if got := string(resp.GetOpaqueBytes()); got != f.want {
						t.Errorf("fetch %d = %q, want %q", i, got, f.want)
					}
				}
				if reads := accessor.calls.Load(); reads != tc.wantReads {
					t.Errorf("Secret Manager reads = %d, want %d", reads, tc.wantReads)
				}
			})
		})
	}
}

// The cache drops a payload when its entry expires, without waiting for another
// read, and holds none while disabled.
func TestFetchSecretCacheEvictsOnExpiry(t *testing.T) {
	const name = "projects/proj-123/secrets/example-api/versions/1"
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		// wait is how long after the fetch the cache's entries are counted.
		wait        time.Duration
		wantEntries int
	}{
		{name: "kept until the ttl passes", ttl: 10 * time.Second, wait: 10*time.Second - time.Nanosecond, wantEntries: 1},
		{name: "dropped once the ttl passes", ttl: 10 * time.Second, wait: 10 * time.Second, wantEntries: 0},
		{name: "none held with the cache disabled", ttl: 0, wantEntries: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				accessor := &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}}
				srv := NewServer(only(accessor), testAuthorizer(t), time.Second, tc.ttl)
				if _, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
					Uri:           "ate-secret://secretmanager.googleapis.com/" + name,
					ActorSpiffeId: testActor,
				}); err != nil {
					t.Fatalf("FetchSecret: %v", err)
				}
				time.Sleep(tc.wait)
				// Let an eviction due at this instant finish.
				synctest.Wait()
				if entries := srv.payloads.Len(); entries != tc.wantEntries {
					t.Errorf("cache entries %v after the fetch = %d, want %d", tc.wait, entries, tc.wantEntries)
				}
			})
		})
	}
}

// The eviction due for an entry that is already gone leaves alone the live
// entry that replaced it.
func TestFetchSecretCacheStaleEvictionKeepsLiveEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const name = "projects/proj-123/secrets/example-api/versions/1"
		accessor := &fakeAccessor{byName: map[string][]byte{name: []byte("s3cr3t")}}
		srv := NewServer(only(accessor), testAuthorizer(t), time.Second, 10*time.Second)
		fetch := func() {
			t.Helper()
			if _, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
				Uri:           "ate-secret://secretmanager.googleapis.com/" + name,
				ActorSpiffeId: testActor,
			}); err != nil {
				t.Fatalf("FetchSecret: %v", err)
			}
		}

		// Stored at 0s, with its eviction due at 10s.
		fetch()
		// Lose the entry early, as an eviction racing a store can.
		srv.payloads.Delete(name)
		time.Sleep(5 * time.Second)
		// Stored at 5s, live until 15s.
		fetch()
		time.Sleep(5 * time.Second)
		// Let the first entry's eviction, due at this instant, finish.
		synctest.Wait()
		fetch()
		if reads := accessor.calls.Load(); reads != 2 {
			t.Errorf("Secret Manager reads = %d, want 2: the first entry's eviction dropped the second", reads)
		}
	})
}

// Secrets that share an ID but differ in project, location or version are kept
// apart, so none is served another's payload: each has its own cache entry and,
// fetched concurrently, its own read.
func TestFetchSecretKeyedByResourceName(t *testing.T) {
	authz, err := newProjectAuthorizer(projectPolicyFile{Policies: []atespaceProjectPolicy{
		{Atespace: "team-a", AllowedProjects: []string{"proj-123", "proj-456"}},
	}})
	if err != nil {
		t.Fatalf("newProjectAuthorizer: %v", err)
	}
	names := []string{
		"projects/proj-123/secrets/example-api/versions/1",
		"projects/proj-123/secrets/example-api/versions/2",
		"projects/proj-123/secrets/example-api/versions/latest",
		"projects/proj-456/secrets/example-api/versions/1",
		"projects/proj-123/locations/us-central1/secrets/example-api/versions/1",
	}
	// newServer returns a server over names and the fake behind it, which
	// serves every location and holds its answers behind gate, if set.
	newServer := func(ttl time.Duration, gate chan struct{}) (*Server, *fakeAccessor) {
		accessor := &fakeAccessor{byName: map[string][]byte{}, gate: gate}
		for _, name := range names {
			accessor.byName[name] = []byte("payload of " + name)
		}
		clients := func(string) (SecretAccessor, error) { return accessor, nil }
		return NewServer(clients, authz, time.Second, ttl), accessor
	}
	fetch := func(srv *Server, name string) (string, error) {
		resp, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
			Uri:           "ate-secret://secretmanager.googleapis.com/" + name,
			ActorSpiffeId: testActor,
		})
		return string(resp.GetOpaqueBytes()), err
	}

	t.Run("cache", func(t *testing.T) {
		srv, accessor := newServer(time.Hour, nil)
		// The second round is served from the cache.
		for round := range 2 {
			for _, name := range names {
				got, err := fetch(srv, name)
				if err != nil {
					t.Fatalf("round %d: FetchSecret(%s): %v", round, name, err)
				}
				if want := "payload of " + name; got != want {
					t.Errorf("round %d: FetchSecret(%s) = %q, want %q", round, name, got, want)
				}
			}
		}
		if reads := int(accessor.calls.Load()); reads != len(names) {
			t.Errorf("Secret Manager reads = %d, want %d, one per secret version", reads, len(names))
		}
	})

	t.Run("concurrent reads", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			srv, accessor := newServer(0, make(chan struct{}))
			type result struct {
				value string
				err   error
			}
			got := make([]result, len(names))
			var wg sync.WaitGroup
			for i, name := range names {
				wg.Go(func() {
					value, err := fetch(srv, name)
					got[i] = result{value, err}
				})
			}
			// Every fetch is now parked on a read, so all the reads are in
			// flight at once.
			synctest.Wait()
			close(accessor.gate)
			wg.Wait()
			for i, name := range names {
				if got[i].err != nil {
					t.Errorf("FetchSecret(%s): %v", name, got[i].err)
					continue
				}
				if want := "payload of " + name; got[i].value != want {
					t.Errorf("FetchSecret(%s) = %q, want %q", name, got[i].value, want)
				}
			}
			if reads := int(accessor.calls.Load()); reads != len(names) {
				t.Errorf("Secret Manager reads = %d, want %d, one per secret version", reads, len(names))
			}
		})
	})
}

// Concurrent fetches of one secret version share a single read, even with the
// cache disabled.
func TestFetchSecretCollapsesConcurrentReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const name = "projects/proj-123/secrets/example-api/versions/1"
		accessor := &fakeAccessor{
			byName: map[string][]byte{name: []byte("s3cr3t")},
			gate:   make(chan struct{}),
		}
		srv := NewServer(only(accessor), testAuthorizer(t), time.Second, 0)

		const callers = 8
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		for range callers {
			wg.Go(func() {
				_, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{
					Uri:           "ate-secret://secretmanager.googleapis.com/" + name,
					ActorSpiffeId: testActor,
				})
				errs <- err
			})
		}
		// Every caller is now parked on the flight, so none can arrive after
		// the read completes and start its own.
		synctest.Wait()
		close(accessor.gate)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("FetchSecret: %v", err)
			}
		}
		if reads := accessor.calls.Load(); reads != 1 {
			t.Errorf("Secret Manager reads = %d, want 1 for %d concurrent callers", reads, callers)
		}
	})
}

// A caller that goes away gets an error of its own and does not fail a caller
// sharing its read.
func TestFetchSecretReadOutlivesItsFirstCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const name = "projects/proj-123/secrets/example-api/versions/1"
		accessor := &fakeAccessor{
			byName: map[string][]byte{name: []byte("s3cr3t")},
			gate:   make(chan struct{}),
		}
		srv := NewServer(only(accessor), testAuthorizer(t), time.Second, 0)
		req := &credprovider.FetchSecretRequest{
			Uri:           "ate-secret://secretmanager.googleapis.com/" + name,
			ActorSpiffeId: testActor,
		}

		// The first caller starts the read; the second joins it.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		firstErr := make(chan error, 1)
		go func() {
			_, err := srv.FetchSecret(ctx, req)
			firstErr <- err
		}()
		synctest.Wait()
		type result struct {
			value string
			err   error
		}
		second := make(chan result, 1)
		go func() {
			resp, err := srv.FetchSecret(context.Background(), req)
			second <- result{string(resp.GetOpaqueBytes()), err}
		}()
		synctest.Wait()

		cancel()
		// Canceled is not a code the gateway retries; Unavailable is.
		if err := <-firstErr; status.Code(err) != codes.Unavailable {
			t.Errorf("canceled caller: code = %v, want Unavailable (err=%v)", status.Code(err), err)
		}
		close(accessor.gate)
		if got := <-second; got.err != nil || got.value != "s3cr3t" {
			t.Errorf("joined caller = %q, %v, want s3cr3t", got.value, got.err)
		}
		if reads := accessor.calls.Load(); reads != 1 {
			t.Errorf("Secret Manager reads = %d, want 1", reads)
		}
	})
}

// Each secret is read through its location's client, by its full resource name.
func TestFetchSecretRoutesByLocation(t *testing.T) {
	const global = "projects/proj-123/secrets/example-api/versions/1"
	const regional = "projects/proj-123/locations/us-central1/secrets/example-api/versions/1"
	globalClient := &fakeAccessor{byName: map[string][]byte{global: []byte("from-global")}}
	regionalClient := &fakeAccessor{byName: map[string][]byte{regional: []byte("from-us-central1")}}
	clients := func(location string) (SecretAccessor, error) {
		switch location {
		case "":
			return globalClient, nil
		case "us-central1":
			return regionalClient, nil
		}
		return nil, fmt.Errorf("cannot create a client for %q", location)
	}
	srv := NewServer(clients, testAuthorizer(t), time.Second, 0)

	for _, tc := range []struct {
		name     string
		uri      string
		want     string
		wantCode codes.Code
	}{
		{name: "global", uri: "ate-secret://secretmanager.googleapis.com/" + global, want: "from-global"},
		{name: "regional", uri: "ate-secret://secretmanager.googleapis.com/" + regional, want: "from-us-central1"},
		// A region whose client cannot be created may work on a retry.
		{name: "no client for the region", uri: "ate-secret://secretmanager.googleapis.com/projects/proj-123/locations/europe-west4/secrets/example-api/versions/1", wantCode: codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.FetchSecret(context.Background(), &credprovider.FetchSecretRequest{Uri: tc.uri, ActorSpiffeId: testActor})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", status.Code(err), tc.wantCode, err)
			}
			if got := string(resp.GetOpaqueBytes()); got != tc.want {
				t.Errorf("FetchSecret = %q, want %q", got, tc.want)
			}
		})
	}
	if globalClient.gotName != global || regionalClient.gotName != regional {
		t.Errorf("global client read %q and regional client %q, want %q and %q", globalClient.gotName, regionalClient.gotName, global, regional)
	}
}
