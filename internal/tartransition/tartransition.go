// Package tartransition is a short-lived internal bridge across accelerator
// issue #27's breaking tarstream digest API change. It keeps the five-repository
// workspace buildable while accelerator and sandboxer merge independently.
// Sandboxer issue #24 must delete this package after adopting the final API.
package tartransition

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// WriteTo calls tarstream.WriteTo through the temporary signature boundary and
// returns the legacy tagged form used by current sandboxer code. It accepts the
// pre-#27 (taggedDigest, error) result and the final #27
// (scheme, digest, error) result. No tarstream options are passed, so behavior
// remains the existing plaintext SHA-256 path until #24 removes this bridge.
func WriteTo(ctx context.Context, w io.Writer, name string, src sparse.Source) (string, error) {
	return callWriteTo(tarstream.WriteTo, ctx, w, name, src)
}

func callWriteTo(fn any, ctx context.Context, w io.Writer, name string, src sparse.Source) (tagged string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			tagged = ""
			err = fmt.Errorf("tartransition: invoke WriteTo: %v", recovered)
		}
	}()

	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return "", fmt.Errorf("tartransition: WriteTo is %T, not a function", fn)
	}
	t := v.Type()
	if t.NumIn() != 4 && !(t.NumIn() == 5 && t.IsVariadic()) {
		return "", fmt.Errorf("tartransition: unsupported WriteTo input signature %s", t)
	}
	args := []reflect.Value{
		reflect.ValueOf(ctx),
		reflect.ValueOf(w),
		reflect.ValueOf(name),
		reflect.ValueOf(src),
	}
	for i := range args {
		if !args[i].IsValid() || !args[i].Type().AssignableTo(t.In(i)) {
			return "", fmt.Errorf("tartransition: WriteTo argument %d has type %T, want %s", i, args[i].Interface(), t.In(i))
		}
	}
	results := v.Call(args)
	switch len(results) {
	case 2:
		if err := resultError(results[1]); err != nil {
			return "", err
		}
		if results[0].Kind() != reflect.String {
			return "", fmt.Errorf("tartransition: legacy WriteTo digest result is %s", results[0].Type())
		}
		return validateTagged(results[0].String())
	case 3:
		if err := resultError(results[2]); err != nil {
			return "", err
		}
		if results[0].Kind() != reflect.String || results[1].Kind() != reflect.String {
			return "", fmt.Errorf("tartransition: final WriteTo digest results are %s and %s", results[0].Type(), results[1].Type())
		}
		return joinDigest(results[0].String(), results[1].String())
	default:
		return "", fmt.Errorf("tartransition: unsupported WriteTo result count %d", len(results))
	}
}

// Digest reads either the pre-#27 Digest() string method or the final #27
// Digest() (scheme, digest string) method without importing either interface.
// A missing method is reported through ok=false; a present but invalid method
// fails closed.
func Digest(source any) (tagged string, ok bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			tagged, ok = "", false
			err = fmt.Errorf("tartransition: invoke Digest: %v", recovered)
		}
	}()

	if source == nil {
		return "", false, nil
	}
	method := reflect.ValueOf(source).MethodByName("Digest")
	if !method.IsValid() {
		return "", false, nil
	}
	if method.Type().NumIn() != 0 {
		return "", false, fmt.Errorf("tartransition: unsupported Digest input signature %s", method.Type())
	}
	results := method.Call(nil)
	switch len(results) {
	case 1:
		if results[0].Kind() != reflect.String {
			return "", false, fmt.Errorf("tartransition: legacy Digest result is %s", results[0].Type())
		}
		tagged, err := validateTagged(results[0].String())
		return tagged, err == nil, err
	case 2:
		if results[0].Kind() != reflect.String || results[1].Kind() != reflect.String {
			return "", false, fmt.Errorf("tartransition: final Digest results are %s and %s", results[0].Type(), results[1].Type())
		}
		tagged, err := joinDigest(results[0].String(), results[1].String())
		return tagged, err == nil, err
	default:
		return "", false, fmt.Errorf("tartransition: unsupported Digest result count %d", len(results))
	}
}

func resultError(v reflect.Value) error {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if !v.IsValid() || !v.Type().Implements(errorType) {
		return fmt.Errorf("tartransition: error result has type %s", v.Type())
	}
	if v.IsNil() {
		return nil
	}
	return v.Interface().(error)
}

func joinDigest(scheme, digest string) (string, error) {
	return validateTagged(scheme + ":" + digest)
}

// SHA256Digest returns the untagged digest only when tagged uses the legacy
// SHA-256 scheme that current sandboxer refs and content-addressed names can
// represent. Final-API HMAC identities must fail closed until issue #24 makes
// those callers scheme-aware and deletes this bridge.
func SHA256Digest(tagged string) (string, error) {
	tagged, err := validateTagged(tagged)
	if err != nil {
		return "", err
	}
	scheme, digest, _ := strings.Cut(tagged, ":")
	if scheme != digestSchemeSHA256 {
		return "", fmt.Errorf("tartransition: digest scheme is not supported before sandboxer issue #24")
	}
	return digest, nil
}

// SHA256RefString formats a legacy SHA-256 file ref across manifest.Ref's
// issue #27 transition. Current Ref derives @sha256 from Digest alone; the
// final Ref also requires DigestScheme. Reflection keeps this temporary bridge
// source-compatible with both definitions.
func SHA256RefString(ref manifest.Ref, digest string) (string, error) {
	if _, err := SHA256Digest(digestSchemeSHA256 + ":" + digest); err != nil {
		return "", err
	}
	ref.Digest = digest
	field := reflect.ValueOf(&ref).Elem().FieldByName("DigestScheme")
	if field.IsValid() {
		if field.Kind() != reflect.String || !field.CanSet() {
			return "", fmt.Errorf("tartransition: unsupported manifest.Ref DigestScheme field")
		}
		field.SetString(digestSchemeSHA256)
	}
	if err := ref.Validate(); err != nil {
		return "", fmt.Errorf("tartransition: validate SHA-256 ref: %w", err)
	}
	return ref.String(), nil
}

const digestSchemeSHA256 = "sha256"

func validateTagged(tagged string) (string, error) {
	scheme, digest, found := strings.Cut(tagged, ":")
	if !found || scheme != digestSchemeSHA256 && scheme != "hmac" {
		return "", fmt.Errorf("tartransition: invalid digest scheme")
	}
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", fmt.Errorf("tartransition: invalid %s digest", scheme)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("tartransition: invalid %s digest", scheme)
	}
	return tagged, nil
}
