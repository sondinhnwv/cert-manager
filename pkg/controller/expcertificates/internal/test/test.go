/*
Copyright 2020 The Jetstack cert-manager contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclock "k8s.io/utils/clock/testing"

	apiutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	certificates "github.com/jetstack/cert-manager/pkg/controller/expcertificates"
	"github.com/jetstack/cert-manager/pkg/util/pki"
	"github.com/jetstack/cert-manager/test/unit/gen"
)

var (
	certificateGvk = cmapi.SchemeGroupVersion.WithKind("Certificate")
)

type CryptoBundle struct {
	// certificate is the Certificate resource used to create this bundle
	Certificate *cmapi.Certificate
	// expectedRequestName is the name of the CertificateRequest that is
	// expected to be created to issue this certificate
	ExpectedRequestName string

	// privateKey is the private key used as the complement to the certificates
	// in this bundle
	PrivateKey      crypto.Signer
	PrivateKeyBytes []byte

	// csr is the CSR used to obtain the certificate in this bundle
	CSR      *x509.CertificateRequest
	CSRBytes []byte

	// certificateRequest is the request that is expected to be created to
	// obtain a certificate when using this bundle
	CertificateRequest                     *cmapi.CertificateRequest
	CertificateRequestPending              *cmapi.CertificateRequest
	CertificateRequestReady                *cmapi.CertificateRequest
	CertificateRequestFailed               *cmapi.CertificateRequest
	CertificateRequestFailedInvalidRequest *cmapi.CertificateRequest

	// cert is a signed certificate
	Cert      *x509.Certificate
	CertBytes []byte

	LocalTemporaryCertificateBytes []byte

	FixedClock *fakeclock.FakeClock
}

func MustCreateCryptoBundle(t *testing.T, crt *cmapi.Certificate, fixedClock *fakeclock.FakeClock) CryptoBundle {
	c, err := createCryptoBundle(crt, fixedClock)
	if err != nil {
		t.Fatalf("error generating crypto bundle: %v", err)
	}
	return *c
}

func createCryptoBundle(crt *cmapi.Certificate, fixedClock *fakeclock.FakeClock) (*CryptoBundle, error) {
	reqName, err := apiutil.ComputeCertificateRequestName(crt)
	if err != nil {
		return nil, err
	}

	privateKey, err := pki.GeneratePrivateKeyForCertificate(crt)
	if err != nil {
		return nil, err
	}

	privateKeyBytes, err := pki.EncodePrivateKey(privateKey, crt.Spec.KeyEncoding)
	if err != nil {
		return nil, err
	}

	csrPEM, err := generateCSRImpl(crt, privateKeyBytes)
	if err != nil {
		return nil, err
	}

	csr, err := pki.DecodeX509CertificateRequestBytes(csrPEM)
	if err != nil {
		return nil, err
	}

	annotations := make(map[string]string)
	for k, v := range crt.Annotations {
		annotations[k] = v
	}
	if crt.Status.Revision != nil {
		annotations[cmapi.CertificateRequestRevisionAnnotationKey] = fmt.Sprintf("%d", crt.Status.Revision)
	}

	annotations[cmapi.CertificateRequestPrivateKeyAnnotationKey] = crt.Spec.SecretName
	annotations[cmapi.CertificateNameKey] = crt.Name
	certificateRequest := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            reqName,
			Namespace:       crt.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(crt, certificateGvk)},
			Annotations:     annotations,
		},
		Spec: cmapi.CertificateRequestSpec{
			CSRPEM:    csrPEM,
			Duration:  crt.Spec.Duration,
			IssuerRef: crt.Spec.IssuerRef,
			IsCA:      crt.Spec.IsCA,
		},
	}

	unsignedCert, err := pki.GenerateTemplateFromCertificateRequest(certificateRequest)
	if err != nil {
		return nil, err
	}

	certBytes, cert, err := pki.SignCertificate(unsignedCert, unsignedCert, privateKey.Public(), privateKey)
	if err != nil {
		return nil, err
	}

	certificateRequestReady := gen.CertificateRequestFrom(certificateRequest,
		gen.SetCertificateRequestCertificate(certBytes),
		gen.SetCertificateRequestStatusCondition(cmapi.CertificateRequestCondition{
			Type:   cmapi.CertificateRequestConditionReady,
			Status: cmmeta.ConditionTrue,
			Reason: cmapi.CertificateRequestReasonIssued,
		}),
	)

	certificateRequestPending := gen.CertificateRequestFrom(certificateRequest,
		gen.SetCertificateRequestStatusCondition(cmapi.CertificateRequestCondition{
			Type:   cmapi.CertificateRequestConditionReady,
			Status: cmmeta.ConditionFalse,
			Reason: cmapi.CertificateRequestReasonPending,
		}),
	)

	certificateRequestFailed := gen.CertificateRequestFrom(certificateRequest,
		gen.SetCertificateRequestStatusCondition(cmapi.CertificateRequestCondition{
			Type:   cmapi.CertificateRequestConditionReady,
			Status: cmmeta.ConditionFalse,
			Reason: cmapi.CertificateRequestReasonFailed,
		}),
	)

	certificateRequestFailedInvalidRequest := gen.CertificateRequestFrom(certificateRequestFailed,
		gen.SetCertificateRequestStatusCondition(cmapi.CertificateRequestCondition{
			Type:   cmapi.CertificateRequestConditionInvalidRequest,
			Status: cmmeta.ConditionTrue,
			Reason: cmapi.CertificateRequestReasonFailed,
		}),
	)

	tempCertBytes, err := certificates.GenerateLocallySignedTemporaryCertificate(crt, privateKeyBytes)
	if err != nil {
		panic("failed to generate test fixture: " + err.Error())
	}

	return &CryptoBundle{
		Certificate:                            crt,
		ExpectedRequestName:                    reqName,
		PrivateKey:                             privateKey,
		PrivateKeyBytes:                        privateKeyBytes,
		CSR:                                    csr,
		CSRBytes:                               csrPEM,
		CertificateRequest:                     certificateRequest,
		CertificateRequestPending:              certificateRequestPending,
		CertificateRequestReady:                certificateRequestReady,
		CertificateRequestFailed:               certificateRequestFailed,
		CertificateRequestFailedInvalidRequest: certificateRequestFailedInvalidRequest,
		Cert:                                   cert,
		CertBytes:                              certBytes,
		LocalTemporaryCertificateBytes:         tempCertBytes,
		FixedClock:                             fixedClock,
	}, nil
}

func generateCSRImpl(crt *cmapi.Certificate, pk []byte) ([]byte, error) {
	csr, err := pki.GenerateCSR(crt)
	if err != nil {
		return nil, err
	}

	signer, err := pki.DecodePrivateKeyBytes(pk)
	if err != nil {
		return nil, err
	}

	csrDER, err := pki.EncodeCSR(csr, signer)
	if err != nil {
		return nil, err
	}

	csrPEM := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE REQUEST", Bytes: csrDER,
	})

	return csrPEM, nil
}