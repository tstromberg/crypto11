// Copyright 2016, 2017 Thales e-Security, Inc
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// "Software"), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
// LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
// WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

// Package crypto11 enables access to cryptographic keys from PKCS#11 using Go crypto API.
//
// Simple use
//
// 1. Either write a configuration file (see ConfigureFromFile) or
// define a configuration in your application (see PKCS11Config and
// Configure). This will identify the PKCS#11 library and token to
// use, and contain the password (or "PIN" in PKCS#11 terminology) to
// use if the token requires login.
//
// 2. Create keys with GenerateDSAKeyPair, GenerateRSAKeyPair and
// GenerateECDSAKeyPair. The keys you get back implement the standard
// Go crypto.Signer interface (and crypto.Decrypter, for RSA). They
// are automatically persisted under random a randomly generated label
// and ID (use the Identify method to discover them).
//
// 3. Retrieve existing keys with FindKeyPair. The return value is a
// Go crypto.PrivateKey; it may be converted either to crypto.Signer
// or to *PKCS11PrivateKeyDSA, *PKCS11PrivateKeyECDSA or
// *PKCS11PrivateKeyRSA.
//
// Sessions and concurrency
//
// Note that PKCS#11 session handles must not be used concurrently
// from multiple threads. Consumers of the Signer interface know
// nothing of this and expect to be able to sign from multiple threads
// without constraint. We address this as follows.
//
// 1. pkcs11Object captures both the object handle and the slot ID
// for an object.
//
// 2. For each slot we maintain a pool of read-write sessions. The
// pool expands dynamically up to an (undocumented) limit.
//
// 3. Each operation transiently takes a session from the pool. They
// have exclusive use of the session, meeting PKCS#11's concurrency
// requirements.
//
// The details are, partially, exposed in the API; since the target
// use case is PKCS#11-unaware operation it may be that the API as it
// stands isn't good enough for PKCS#11-aware applications. Feedback
// welcome.
//
// See also https://golang.org/pkg/crypto/
//
// Limitations
//
// The PKCS1v15DecryptOptions SessionKeyLen field is not implemented
// and an error is returned if it is nonzero.
// The reason for this is that it is not possible for crypto11 to guarantee the constant-time behavior in the specification.
// See https://github.com/thalesignite/crypto11/issues/5 for further discussion.
//
// Symmetric crypto support via cipher.Block is very slow.
// You can use the BlockModeCloser API
// but you must call the Close() interface (not found in cipher.BlockMode).
// See https://github.com/ThalesIgnite/crypto11/issues/6 for further discussion.
package crypto11

import (
	"crypto"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/miekg/pkcs11"
	"github.com/pkg/errors"
	"github.com/vitessio/vitess/go/pools"
)

const (
	// DefaultMaxSessions controls the maximum number of concurrent sessions to
	// open, unless otherwise specified in the PKCS11Config object.
	DefaultMaxSessions = 1024
)

// ErrTokenNotFound represents the failure to find the requested PKCS#11 token
var ErrTokenNotFound = errors.New("crypto11: could not find PKCS#11 token")

// ErrKeyNotFound represents the failure to find the requested PKCS#11 key
var ErrKeyNotFound = errors.New("crypto11: could not find PKCS#11 key")

// ErrCannotOpenPKCS11 is returned when the PKCS#11 library cannot be opened
var ErrCannotOpenPKCS11 = errors.New("crypto11: could not open PKCS#11")

// ErrCannotGetRandomData is returned when the PKCS#11 library fails to return enough random data
var ErrCannotGetRandomData = errors.New("crypto11: cannot get random data from PKCS#11")

// ErrUnsupportedKeyType is returned when the PKCS#11 library returns a key type that isn't supported
var ErrUnsupportedKeyType = errors.New("crypto11: unrecognized key type")

// pkcs11Object contains a reference to a loaded PKCS#11 object.
type pkcs11Object struct {
	// TODO - handle resource cleanup. Consider adding explicit Close method and/or use a finalizer

	// The PKCS#11 object handle.
	handle pkcs11.ObjectHandle

	// The PKCS#11 context. This is used  to find a session handle that can
	// access this object.
	context *Context
}

// pkcs11PrivateKey contains a reference to a loaded PKCS#11 private key object.
type pkcs11PrivateKey struct {
	pkcs11Object

	// The corresponding public key
	pubKey crypto.PublicKey

	// In a former design we carried around the object handle for the
	// public key and retrieved it on demand.  The downside of that is
	// that the Public() method on Signer &c has no way to communicate
	// errors.
}

// A Context stores the connection state to a PKCS#11 token. Use Configure or ConfigureFromFile to create a new
// Context. Call Close when finished with the token, to free up resources.
//
// All functions, except Close, are safe to call from multiple goroutines.
type Context struct {
	ctx *pkcs11.Ctx
	cfg *PKCS11Config

	token *pkcs11.TokenInfo
	slot  uint
	pool  *pools.ResourcePool
}

// findToken finds a token given its serial number
func (c *Context) findToken(slots []uint, serial string, label string) (uint, *pkcs11.TokenInfo, error) {
	for _, slot := range slots {
		tokenInfo, err := c.ctx.GetTokenInfo(slot)
		if err != nil {
			return 0, nil, err
		}
		if tokenInfo.SerialNumber == serial {
			return slot, &tokenInfo, nil
		}
		if tokenInfo.Label == label {
			return slot, &tokenInfo, nil
		}
	}
	return 0, nil, ErrTokenNotFound
}

// PKCS11Config holds PKCS#11 configuration information.
//
// A token may be identified either by serial number or label.  If
// both are specified then the first match wins.
//
// Supply this to Configure(), or alternatively use ConfigureFromFile().
type PKCS11Config struct {
	// Full path to PKCS#11 library
	Path string

	// Token serial number
	TokenSerial string

	// Token label
	TokenLabel string

	// User PIN (password)
	Pin string

	// Maximum number of concurrent sessions to open
	MaxSessions int

	// Session idle timeout to be evicted from the pool
	IdleTimeout time.Duration

	// Maximum time allowed to wait a sessions pool for a session
	PoolWaitTimeout time.Duration
}

// Configure configures PKCS#11 from a PKCS11Config.
//
// The PKCS#11 library context is returned,
// allowing a PKCS#11-aware application to make use of it. Non-aware
// appliations may ignore it.
//
// Unsually, these values may be present even if the error is
// non-nil. This corresponds to the case that the library has already
// been configured. Note that it is NOT reconfigured so if you supply
// a different configuration the second time, it will be ignored in
// favor of the first configuration.
//
// If config is nil, and the library has already been configured, the
// context from the first configuration is returned (and
// the error will be nil in this case).
func Configure(config *PKCS11Config) (*Context, error) {
	if config.MaxSessions == 0 {
		config.MaxSessions = DefaultMaxSessions
	}

	instance := &Context{
		cfg: config,
		ctx: pkcs11.New(config.Path),
	}

	if instance.ctx == nil {
		log.Printf("Could not open PKCS#11 library: %s", config.Path)
		return nil, ErrCannotOpenPKCS11
	}
	if err := instance.ctx.Initialize(); err != nil {
		log.Printf("Failed to initialize PKCS#11 library: %s", err.Error())
		return nil, err
	}
	slots, err := instance.ctx.GetSlotList(true)
	if err != nil {
		log.Printf("Failed to list PKCS#11 Slots: %s", err.Error())
		return nil, err
	}

	instance.slot, instance.token, err = instance.findToken(slots, config.TokenSerial, config.TokenLabel)
	if err != nil {
		log.Printf("Failed to find Token in any Slot: %s", err.Error())
		return nil, err
	}

	// TODO - why is this an error condition? 'Max' implies an upperbound, not a requirement. We could take the
	// smaller of these two values.
	if instance.token.MaxRwSessionCount > 0 && uint(instance.cfg.MaxSessions) > instance.token.MaxRwSessionCount {
		return nil, fmt.Errorf("crypto11: provided max sessions value (%d) exceeds max value the token supports (%d)",
			instance.cfg.MaxSessions, instance.token.MaxRwSessionCount)
	}

	instance.pool = pools.NewResourcePool(instance.resourcePoolFactoryFunc, config.MaxSessions,
		config.MaxSessions, config.IdleTimeout)

	return instance, nil
}

// ConfigureFromFile configures PKCS#11 from a name configuration file.
//
// Configuration files are a JSON representation of the PKCSConfig object.
// The return value is as for Configure().
func ConfigureFromFile(configLocation string) (*Context, error) {
	config, err := loadConfigFromFile(configLocation)
	if err != nil {
		return nil, err
	}

	return Configure(config)
}

// loadConfigFromFile reads a PKCS11Config struct from a file.
func loadConfigFromFile(configLocation string) (*PKCS11Config, error) {
	file, err := os.Open(configLocation)
	if err != nil {
		return nil, errors.WithMessagef(err, "could not open config file: %s", configLocation)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	configDecoder := json.NewDecoder(file)
	config := &PKCS11Config{}
	err = configDecoder.Decode(config)
	return config, errors.WithMessage(err, "could decode config file:")
}

// Close waits for existing operations to finish, before releasing all the resources used by the Context (and unloading
// the underlying PKCS #11 library). A closed Context cannot be reused.
func (c *Context) Close() error {

	// Blocks until all resources returned to pool
	c.pool.Close()

	err := c.ctx.Finalize()
	if err != nil {
		return err
	}

	c.ctx.Destroy()
	return nil
}
