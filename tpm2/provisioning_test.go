// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package tpm2_test

import (
	"bytes"
	"testing"

	"github.com/canonical/go-tpm2"
	"github.com/canonical/go-tpm2/mu"
	tpm2_testutil "github.com/canonical/go-tpm2/testutil"

	"golang.org/x/xerrors"

	"github.com/snapcore/secboot/internal/tcg"
	"github.com/snapcore/secboot/internal/tpm2test"
	. "github.com/snapcore/secboot/tpm2"
)

func validatePrimaryKeyAgainstTemplate(t *testing.T, tpm *tpm2.TPMContext, hierarchy, handle tpm2.Handle, template *tpm2.Public) {
	key, err := tpm.CreateResourceContextFromTPM(handle)
	if err != nil {
		t.Errorf("Cannot create context: %v", err)
	}

	// The easiest way to validate that the primary key was created with the supplied
	// template is to just create it again and compare the names
	expected, _, _, _, _, err := tpm.CreatePrimary(tpm.GetPermanentContext(hierarchy), nil, template, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreatePrimary failed: %v", err)
	}
	defer tpm.FlushContext(expected)

	if !bytes.Equal(key.Name(), expected.Name()) {
		t.Errorf("Key doesn't match")
	}
}

func validateSRK(t *testing.T, tpm *tpm2.TPMContext) {
	validatePrimaryKeyAgainstTemplate(t, tpm, tpm2.HandleOwner, tcg.SRKHandle, tcg.SRKTemplate)
}

func validateEK(t *testing.T, tpm *tpm2.TPMContext) {
	validatePrimaryKeyAgainstTemplate(t, tpm, tpm2.HandleEndorsement, tcg.EKHandle, tcg.EKTemplate)
}

func TestProvisionNewTPM(t *testing.T) {
	for _, data := range []struct {
		desc string
		mode ProvisionMode
	}{
		{
			desc: "Clear",
			mode: ProvisionModeClear,
		},
		{
			desc: "Full",
			mode: ProvisionModeFull,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureClear|
					tpm2test.TPMFeatureNV)
			defer closeTPM()

			lockoutAuth := []byte("1234")

			origEk, _ := tpm.EndorsementKey()
			origHmacSession := tpm.HmacSession()

			if err := tpm.EnsureProvisioned(data.mode, lockoutAuth); err != nil {
				t.Errorf("EnsureProvisioned failed: %v", err)
			}
			defer func() {
				// github.com/canonical/go-tpm2/testutil cannot restore this because
				// EnsureProvisioned uses command parameter encryption. We have to do
				// this manually else the test fixture fails
				if err := tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil); err != nil {
					t.Errorf("HierarchyChangeAuth failed: %v", err)
				}
			}()

			validateEK(t, tpm.TPMContext)
			validateSRK(t, tpm.TPMContext)

			// Validate the DA parameters
			props, err := tpm.GetCapabilityTPMProperties(tpm2.PropertyMaxAuthFail, 3)
			if err != nil {
				t.Fatalf("GetCapability failed: %v", err)
			}
			if props[0].Value != uint32(32) || props[1].Value != uint32(7200) ||
				props[2].Value != uint32(86400) {
				t.Errorf("ProvisionTPM didn't set the DA parameters correctly")
			}

			// Verify that owner control is disabled, that the lockout hierarchy auth is set, and no
			// other hierarchy auth is set
			props, err = tpm.GetCapabilityTPMProperties(tpm2.PropertyPermanent, 1)
			if err != nil {
				t.Fatalf("GetCapability failed: %v", err)
			}
			if tpm2.PermanentAttributes(props[0].Value)&tpm2.AttrLockoutAuthSet == 0 {
				t.Errorf("ProvisionTPM didn't set the lockout hierarchy auth")
			}
			if tpm2.PermanentAttributes(props[0].Value)&tpm2.AttrDisableClear == 0 {
				t.Errorf("ProvisionTPM didn't disable owner clear")
			}
			if tpm2.PermanentAttributes(props[0].Value)&(tpm2.AttrOwnerAuthSet|tpm2.AttrEndorsementAuthSet) > 0 {
				t.Errorf("ProvisionTPM returned with authorizations set for owner or endorsement hierarchies")
			}

			// Test the lockout hierarchy auth
			tpm.LockoutHandleContext().SetAuthValue(lockoutAuth)
			if err := tpm.DictionaryAttackLockReset(tpm.LockoutHandleContext(), nil); err != nil {
				t.Errorf("Use of the lockout hierarchy auth failed: %v", err)
			}

			hmacSession := tpm.HmacSession()
			if hmacSession == nil || hmacSession.Handle().Type() != tpm2.HandleTypeHMACSession {
				t.Errorf("Invalid HMAC session handle")
			}
			if hmacSession == origHmacSession {
				t.Errorf("Invalid HMAC session handle")
			}

			ek, err := tpm.EndorsementKey()
			if err != nil {
				t.Fatalf("No EK context: %v", err)
			}
			if ek.Handle().Type() != tpm2.HandleTypePersistent {
				t.Errorf("Invalid EK handle")
			}
			if ek == origEk {
				t.Errorf("Invalid EK handle")
			}

			// Make sure ProvisionTPM didn't leak transient objects
			handles, err := tpm.GetCapabilityHandles(tpm2.HandleTypeTransient.BaseHandle(), tpm2.CapabilityMaxProperties)
			if err != nil {
				t.Fatalf("GetCapability failed: %v", err)
			}
			if len(handles) > 0 {
				t.Errorf("ProvisionTPM leaked transient handles")
			}

			handles, err = tpm.GetCapabilityHandles(tpm2.HandleTypeLoadedSession.BaseHandle(), tpm2.CapabilityMaxProperties)
			if err != nil {
				t.Fatalf("GetCapability failed: %v", err)
			}
			if len(handles) > 1 || (len(handles) > 0 && handles[0] != hmacSession.Handle()) {
				t.Errorf("ProvisionTPM leaked loaded session handles")
			}
		})
	}
}

func TestProvisionErrorHandling(t *testing.T) {
	errEndorsementAuthFail := AuthFailError{Handle: tpm2.HandleEndorsement}
	errOwnerAuthFail := AuthFailError{Handle: tpm2.HandleOwner}
	errLockoutAuthFail := AuthFailError{Handle: tpm2.HandleLockout}

	authValue := []byte("1234")

	setLockoutAuth := func(t *testing.T, tpm *Connection) {
		if err := tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), authValue, nil); err != nil {
			t.Fatalf("HierarchyChangeAuth failed: %v", err)
		}
	}
	disableOwnerClear := func(t *testing.T, tpm *Connection) {
		if err := tpm.ClearControl(tpm.LockoutHandleContext(), true, nil); err != nil {
			t.Fatalf("ClearControl failed: %v", err)
		}
	}

	for _, data := range []struct {
		desc        string
		mode        ProvisionMode
		lockoutAuth []byte
		prepare     func(*testing.T, *Connection)
		err         error
	}{
		{
			desc: "ErrTPMClearRequiresPPI",
			mode: ProvisionModeClear,
			prepare: func(t *testing.T, tpm *Connection) {
				disableOwnerClear(t, tpm)
			},
			err: ErrTPMClearRequiresPPI,
		},
		{
			desc: "ErrTPMLockoutAuthFail/1",
			mode: ProvisionModeFull,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
			},
			lockoutAuth: []byte("5678"),
			err:         errLockoutAuthFail,
		},
		{
			desc: "ErrTPMLockoutAuthFail/2",
			mode: ProvisionModeClear,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
			},
			lockoutAuth: []byte("5678"),
			err:         errLockoutAuthFail,
		},
		{
			desc: "ErrInLockout/1",
			mode: ProvisionModeFull,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
				tpm.LockoutHandleContext().SetAuthValue(nil)
				tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil)
			},
			lockoutAuth: authValue,
			err:         ErrTPMLockout,
		},
		{
			desc: "ErrInLockout/2",
			mode: ProvisionModeClear,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
				tpm.LockoutHandleContext().SetAuthValue(nil)
				tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil)
			},
			lockoutAuth: authValue,
			err:         ErrTPMLockout,
		},
		{
			desc: "ErrOwnerAuthFail",
			mode: ProvisionModeWithoutLockout,
			prepare: func(t *testing.T, tpm *Connection) {
				if err := tpm.HierarchyChangeAuth(tpm.OwnerHandleContext(), authValue, nil); err != nil {
					t.Fatalf("HierarchyChangeAuth failed: %v", err)
				}
			},
			err: errOwnerAuthFail,
		},
		{
			desc: "ErrEndorsementAuthFail",
			mode: ProvisionModeWithoutLockout,
			prepare: func(t *testing.T, tpm *Connection) {
				if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), authValue, nil); err != nil {
					t.Fatalf("HierarchyChangeAuth failed: %v", err)
				}
			},
			err: errEndorsementAuthFail,
		},
		{
			desc: "ErrTPMProvisioningRequiresLockout/1",
			mode: ProvisionModeWithoutLockout,
			err:  ErrTPMProvisioningRequiresLockout,
		},
		{
			desc: "ErrTPMProvisioningRequiresLockout/2",
			mode: ProvisionModeWithoutLockout,
			prepare: func(t *testing.T, tpm *Connection) {
				disableOwnerClear(t, tpm)
			},
			err: ErrTPMProvisioningRequiresLockout,
		},
		{
			desc: "ErrTPMProvisioningRequiresLockout/3",
			mode: ProvisionModeWithoutLockout,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
			},
			err: ErrTPMProvisioningRequiresLockout,
		},
		{
			desc: "ErrTPMProvisioningRequiresLockout/4",
			mode: ProvisionModeWithoutLockout,
			prepare: func(t *testing.T, tpm *Connection) {
				setLockoutAuth(t, tpm)
				disableOwnerClear(t, tpm)
			},
			err: ErrTPMProvisioningRequiresLockout,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureClear|
					tpm2test.TPMFeatureNV)
			defer func() {
				// Some of these tests trip the lockout for the lockout auth,
				// which can't be undone by the test fixture. Clear the TPM
				// else the test fixture fails the test.
				tpm2_testutil.ClearTPMUsingPlatformHierarchyT(t, tpm.TPMContext)
				closeTPM()
			}()

			if data.prepare != nil {
				data.prepare(t, tpm)
			}
			tpm.LockoutHandleContext().SetAuthValue(data.lockoutAuth)
			tpm.OwnerHandleContext().SetAuthValue(nil)
			tpm.EndorsementHandleContext().SetAuthValue(nil)

			err := tpm.EnsureProvisioned(data.mode, nil)
			if err == nil {
				t.Fatalf("EnsureProvisioned should have returned an error")
			}
			if err != data.err {
				t.Errorf("EnsureProvisioned returned an unexpected error: %v", err)
			}
		})
	}
}

func TestRecreateEK(t *testing.T) {
	for _, data := range []struct {
		desc string
		mode ProvisionMode
	}{
		{
			desc: "Full",
			mode: ProvisionModeFull,
		},
		{
			desc: "WithoutLockout",
			mode: ProvisionModeWithoutLockout,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureNV)
			defer closeTPM()

			lockoutAuth := []byte("1234")

			if err := tpm.EnsureProvisioned(ProvisionModeFull, lockoutAuth); err != nil {
				t.Errorf("EnsureProvisioned failed: %v", err)
			}
			defer func() {
				// github.com/canonical/go-tpm2/testutil cannot restore this because
				// EnsureProvisioned uses command parameter encryption. We have to do
				// this manually else the test fixture fails
				if err := tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil); err != nil {
					t.Errorf("HierarchyChangeAuth failed: %v", err)
				}
			}()

			ek, err := tpm.EndorsementKey()
			if err != nil {
				t.Fatalf("No EK context: %v", err)
			}
			if ek.Handle().Type() != tpm2.HandleTypePersistent {
				t.Errorf("Invalid EK handle type")
			}
			hmacSession := tpm.HmacSession()
			if hmacSession == nil || hmacSession.Handle().Type() != tpm2.HandleTypeHMACSession {
				t.Errorf("Invalid HMAC session handle")
			}

			ek, err = tpm.CreateResourceContextFromTPM(tcg.EKHandle)
			if err != nil {
				t.Fatalf("No EK context: %v", err)
			}
			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), ek, ek.Handle(), nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}

			if err := tpm.EnsureProvisioned(data.mode, lockoutAuth); err != nil {
				t.Errorf("EnsureProvisioned failed: %v", err)
			}

			validateEK(t, tpm.TPMContext)

			hmacSession2 := tpm.HmacSession()
			if hmacSession2 == nil || hmacSession2.Handle().Type() != tpm2.HandleTypeHMACSession {
				t.Errorf("Invalid HMAC session handle")
			}
			ek2, err := tpm.EndorsementKey()
			if err != nil {
				t.Fatalf("No EK context: %v", err)
			}
			if ek2.Handle().Type() != tpm2.HandleTypePersistent {
				t.Errorf("Invalid EK handle")
			}
			if hmacSession.Handle() != tpm2.HandleUnassigned {
				t.Errorf("Original HMAC session should have been flushed")
			}
			if ek == ek2 {
				t.Errorf("Original EK context should have been evicted")
			}
		})
	}
}

func TestRecreateSRK(t *testing.T) {
	for _, data := range []struct {
		desc string
		mode ProvisionMode
	}{
		{
			desc: "Full",
			mode: ProvisionModeFull,
		},
		{
			desc: "WithoutLockout",
			mode: ProvisionModeWithoutLockout,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureNV)
			defer closeTPM()

			lockoutAuth := []byte("1234")

			if err := tpm.EnsureProvisioned(ProvisionModeFull, lockoutAuth); err != nil {
				t.Errorf("EnsureProvisioned failed: %v", err)
			}
			defer func() {
				// github.com/canonical/go-tpm2/testutil cannot restore this because
				// EnsureProvisioned uses command parameter encryption. We have to do
				// this manually else the test fixture fails
				if err := tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil); err != nil {
					t.Errorf("HierarchyChangeAuth failed: %v", err)
				}
			}()

			srk, err := tpm.CreateResourceContextFromTPM(tcg.SRKHandle)
			if err != nil {
				t.Fatalf("No SRK context: %v", err)
			}
			expectedName := srk.Name()

			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), srk, srk.Handle(), nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}

			if err := tpm.EnsureProvisioned(data.mode, lockoutAuth); err != nil {
				t.Errorf("EnsureProvisioned failed: %v", err)
			}

			srk, err = tpm.CreateResourceContextFromTPM(tcg.SRKHandle)
			if err != nil {
				t.Fatalf("No SRK context: %v", err)
			}
			if !bytes.Equal(srk.Name(), expectedName) {
				t.Errorf("Unexpected SRK name")
			}

			validateSRK(t, tpm.TPMContext)
		})
	}
}

func TestProvisionWithEndorsementAuth(t *testing.T) {
	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	testAuth := []byte("1234")

	if err := tpm.HierarchyChangeAuth(tpm.EndorsementHandleContext(), testAuth, nil); err != nil {
		t.Fatalf("HierarchyChangeAuth failed: %v", err)
	}

	if err := tpm.EnsureProvisioned(ProvisionModeWithoutLockout, nil); err != ErrTPMProvisioningRequiresLockout {
		t.Fatalf("EnsureProvisioned failed: %v", err)
	}

	validateEK(t, tpm.TPMContext)
	validateSRK(t, tpm.TPMContext)
}

func TestProvisionWithOwnerAuth(t *testing.T) {
	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	testAuth := []byte("1234")

	if err := tpm.HierarchyChangeAuth(tpm.OwnerHandleContext(), testAuth, nil); err != nil {
		t.Fatalf("HierarchyChangeAuth failed: %v", err)
	}

	if err := tpm.EnsureProvisioned(ProvisionModeWithoutLockout, nil); err != ErrTPMProvisioningRequiresLockout {
		t.Fatalf("EnsureProvisioned failed: %v", err)
	}

	validateEK(t, tpm.TPMContext)
	validateSRK(t, tpm.TPMContext)
}

func TestProvisionWithInvalidEkCert(t *testing.T) {
	ConnectToTPM = secureConnectToDefaultTPMHelper
	defer func() { ConnectToTPM = ConnectToDefaultTPM }()

	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	// Temporarily modify the public template so that ProvisionTPM generates a primary key that doesn't match the EK cert
	ekTemplate := tcg.MakeDefaultEKTemplate()
	ekTemplate.Unique.RSA[0] = 0xff
	restore := tpm2test.MockEKTemplate(ekTemplate)
	defer restore()

	err := tpm.EnsureProvisioned(ProvisionModeFull, nil)
	if err == nil {
		t.Fatalf("EnsureProvisioned should have returned an error")
	}
	var ve TPMVerificationError
	if !xerrors.As(err, &ve) && err.Error() != "verification of the TPM failed: cannot verify TPM: endorsement key returned from the "+
		"TPM doesn't match the endorsement certificate" {
		t.Errorf("ProvisionTPM returned an unexpected error: %v", err)
	}
}

func TestProvisionWithCustomSRKTemplate(t *testing.T) {
	for _, data := range []struct {
		desc string
		mode ProvisionMode
	}{
		{
			desc: "Clear",
			mode: ProvisionModeClear,
		},
		{
			desc: "Full",
			mode: ProvisionModeFull,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureClear|
					tpm2test.TPMFeatureNV)
			defer closeTPM()

			template := tpm2.Public{
				Type:    tpm2.ObjectTypeRSA,
				NameAlg: tpm2.HashAlgorithmSHA256,
				Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
					tpm2.AttrRestricted | tpm2.AttrDecrypt,
				Params: &tpm2.PublicParamsU{
					RSADetail: &tpm2.RSAParams{
						Symmetric: tpm2.SymDefObject{
							Algorithm: tpm2.SymObjectAlgorithmAES,
							KeyBits:   &tpm2.SymKeyBitsU{Sym: 128},
							Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
						Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
						KeyBits:  2048,
						Exponent: 0}}}

			if err := tpm.EnsureProvisionedWithCustomSRK(data.mode, nil, &template); err != nil {
				t.Errorf("EnsureProvisionedWithCustomSRK failed: %v", err)
			}

			validatePrimaryKeyAgainstTemplate(t, tpm.TPMContext, tpm2.HandleOwner, tcg.SRKHandle, &template)

			nv, err := tpm.CreateResourceContextFromTPM(0x01810001)
			if err != nil {
				t.Fatalf("CreateResourceContextFromTPM failed: %v", err)
			}

			nvPub, _, err := tpm.NVReadPublic(nv)
			if err != nil {
				t.Fatalf("NVReadPublic failed: %v", err)
			}

			if nvPub.Attrs != tpm2.NVTypeOrdinary.WithAttrs(tpm2.AttrNVAuthWrite|tpm2.AttrNVWriteDefine|tpm2.AttrNVOwnerRead|tpm2.AttrNVNoDA|tpm2.AttrNVWriteLocked|tpm2.AttrNVWritten) {
				t.Errorf("Unexpected attributes")
			}

			tmplB, err := tpm.NVRead(tpm.OwnerHandleContext(), nv, nvPub.Size, 0, nil)
			if err != nil {
				t.Errorf("NVRead failed: %v", err)
			}

			expected, _ := mu.MarshalToBytes(&template)
			if !bytes.Equal(tmplB, expected) {
				t.Errorf("Unexpected template")
			}
		})
	}
}

func TestProvisionWithInvalidCustomSRKTemplate(t *testing.T) {
	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	template := tpm2.Public{
		Type:    tpm2.ObjectTypeRSA,
		NameAlg: tpm2.HashAlgorithmSHA256,
		Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
			tpm2.AttrRestricted | tpm2.AttrSign,
		Params: &tpm2.PublicParamsU{
			RSADetail: &tpm2.RSAParams{
				Symmetric: tpm2.SymDefObject{
					Algorithm: tpm2.SymObjectAlgorithmAES,
					KeyBits:   &tpm2.SymKeyBitsU{Sym: 128},
					Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
				Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
				KeyBits:  2048,
				Exponent: 0}}}
	err := tpm.EnsureProvisionedWithCustomSRK(ProvisionModeWithoutLockout, nil, &template)
	if err == nil {
		t.Fatalf("EnsureProvisionedWithCustomSRK should have failed")
	}

	if err.Error() != "supplied SRK template is not valid for a parent key" {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func TestProvisionDefaultPreservesCustomSRKTemplate(t *testing.T) {
	for _, data := range []struct {
		desc string
		mode ProvisionMode
	}{
		{
			desc: "Full",
			mode: ProvisionModeFull,
		},
		{
			desc: "WithoutLockout",
			mode: ProvisionModeWithoutLockout,
		},
	} {
		t.Run(data.desc, func(t *testing.T) {
			tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
				tpm2test.TPMFeatureOwnerHierarchy|
					tpm2test.TPMFeatureEndorsementHierarchy|
					tpm2test.TPMFeatureLockoutHierarchy|
					tpm2test.TPMFeaturePlatformHierarchy|
					tpm2test.TPMFeatureNV)
			defer closeTPM()

			template := tpm2.Public{
				Type:    tpm2.ObjectTypeRSA,
				NameAlg: tpm2.HashAlgorithmSHA256,
				Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
					tpm2.AttrRestricted | tpm2.AttrDecrypt,
				Params: &tpm2.PublicParamsU{
					RSADetail: &tpm2.RSAParams{
						Symmetric: tpm2.SymDefObject{
							Algorithm: tpm2.SymObjectAlgorithmAES,
							KeyBits:   &tpm2.SymKeyBitsU{Sym: 128},
							Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
						Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
						KeyBits:  2048,
						Exponent: 0}}}

			lockoutAuth := []byte("1234")
			if err := tpm.EnsureProvisionedWithCustomSRK(ProvisionModeFull, lockoutAuth, &template); err != nil {
				t.Errorf("EnsureProvisionedWithCustomSRK failed: %v", err)
			}
			defer func() {
				// github.com/canonical/go-tpm2/testutil cannot restore this because
				// EnsureProvisioned uses command parameter encryption. We have to do
				// this manually else the test fixture fails
				if err := tpm.HierarchyChangeAuth(tpm.LockoutHandleContext(), nil, nil); err != nil {
					t.Errorf("HierarchyChangeAuth failed: %v", err)
				}
			}()

			srk, err := tpm.CreateResourceContextFromTPM(tcg.SRKHandle)
			if err != nil {
				t.Fatalf("No SRK context: %v", err)
			}

			if _, err := tpm.EvictControl(tpm.OwnerHandleContext(), srk, srk.Handle(), nil); err != nil {
				t.Errorf("EvictControl failed: %v", err)
			}

			if err := tpm.EnsureProvisioned(data.mode, lockoutAuth); err != nil {
				t.Fatalf("EnsureProvisioned failed: %v", err)
			}

			validatePrimaryKeyAgainstTemplate(t, tpm.TPMContext, tpm2.HandleOwner, tcg.SRKHandle, &template)
		})
	}
}

func TestProvisionDefaultClearRemovesCustomSRKTemplate(t *testing.T) {
	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureLockoutHierarchy|
			tpm2test.TPMFeaturePlatformHierarchy|
			tpm2test.TPMFeatureClear|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	template := tpm2.Public{
		Type:    tpm2.ObjectTypeRSA,
		NameAlg: tpm2.HashAlgorithmSHA256,
		Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
			tpm2.AttrRestricted | tpm2.AttrDecrypt,
		Params: &tpm2.PublicParamsU{
			RSADetail: &tpm2.RSAParams{
				Symmetric: tpm2.SymDefObject{
					Algorithm: tpm2.SymObjectAlgorithmAES,
					KeyBits:   &tpm2.SymKeyBitsU{Sym: 128},
					Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
				Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
				KeyBits:  2048,
				Exponent: 0}}}

	if err := tpm.EnsureProvisionedWithCustomSRK(ProvisionModeWithoutLockout, nil, &template); err != ErrTPMProvisioningRequiresLockout {
		t.Errorf("EnsureProvisionedWithCustomSRK failed: %v", err)
	}

	validatePrimaryKeyAgainstTemplate(t, tpm.TPMContext, tpm2.HandleOwner, tcg.SRKHandle, &template)

	if err := tpm.EnsureProvisioned(ProvisionModeClear, nil); err != nil {
		t.Fatalf("EnsureProvisioned failed: %v", err)
	}

	validateSRK(t, tpm.TPMContext)
}

func TestProvisionWithCustomSRKTemplateOverwritesExisting(t *testing.T) {
	tpm, _, closeTPM := tpm2test.OpenTPMConnectionT(t,
		tpm2test.TPMFeatureOwnerHierarchy|
			tpm2test.TPMFeatureEndorsementHierarchy|
			tpm2test.TPMFeatureLockoutHierarchy|
			tpm2test.TPMFeaturePlatformHierarchy|
			tpm2test.TPMFeatureNV)
	defer closeTPM()

	template1 := tpm2.Public{
		Type:    tpm2.ObjectTypeRSA,
		NameAlg: tpm2.HashAlgorithmSHA256,
		Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
			tpm2.AttrRestricted | tpm2.AttrDecrypt,
		Params: &tpm2.PublicParamsU{
			RSADetail: &tpm2.RSAParams{
				Symmetric: tpm2.SymDefObject{
					Algorithm: tpm2.SymObjectAlgorithmAES,
					KeyBits:   &tpm2.SymKeyBitsU{Sym: 128},
					Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
				Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
				KeyBits:  2048,
				Exponent: 0}}}

	if err := tpm.EnsureProvisionedWithCustomSRK(ProvisionModeFull, nil, &template1); err != nil {
		t.Errorf("EnsureProvisionedWithCustomSRK failed: %v", err)
	}

	validatePrimaryKeyAgainstTemplate(t, tpm.TPMContext, tpm2.HandleOwner, tcg.SRKHandle, &template1)

	template2 := tpm2.Public{
		Type:    tpm2.ObjectTypeRSA,
		NameAlg: tpm2.HashAlgorithmSHA256,
		Attrs: tpm2.AttrFixedTPM | tpm2.AttrFixedParent | tpm2.AttrSensitiveDataOrigin | tpm2.AttrUserWithAuth | tpm2.AttrNoDA |
			tpm2.AttrRestricted | tpm2.AttrDecrypt,
		Params: &tpm2.PublicParamsU{
			RSADetail: &tpm2.RSAParams{
				Symmetric: tpm2.SymDefObject{
					Algorithm: tpm2.SymObjectAlgorithmAES,
					KeyBits:   &tpm2.SymKeyBitsU{Sym: 256},
					Mode:      &tpm2.SymModeU{Sym: tpm2.SymModeCFB}},
				Scheme:   tpm2.RSAScheme{Scheme: tpm2.RSASchemeNull},
				KeyBits:  2048,
				Exponent: 0}}}

	if err := tpm.EnsureProvisionedWithCustomSRK(ProvisionModeFull, nil, &template2); err != nil {
		t.Errorf("EnsureProvisionedWithCustomSRK failed: %v", err)
	}

	validatePrimaryKeyAgainstTemplate(t, tpm.TPMContext, tpm2.HandleOwner, tcg.SRKHandle, &template2)

	nv, err := tpm.CreateResourceContextFromTPM(0x01810001)
	if err != nil {
		t.Fatalf("CreateResourceContextFromTPM failed: %v", err)
	}

	nvPub, _, err := tpm.NVReadPublic(nv)
	if err != nil {
		t.Fatalf("NVReadPublic failed: %v", err)
	}

	if nvPub.Attrs != tpm2.NVTypeOrdinary.WithAttrs(tpm2.AttrNVAuthWrite|tpm2.AttrNVWriteDefine|tpm2.AttrNVOwnerRead|tpm2.AttrNVNoDA|tpm2.AttrNVWriteLocked|tpm2.AttrNVWritten) {
		t.Errorf("Unexpected attributes")
	}

	tmplB, err := tpm.NVRead(tpm.OwnerHandleContext(), nv, nvPub.Size, 0, nil)
	if err != nil {
		t.Errorf("NVRead failed: %v", err)
	}

	expected, _ := mu.MarshalToBytes(&template2)
	if !bytes.Equal(tmplB, expected) {
		t.Errorf("Unexpected template")
	}
}
