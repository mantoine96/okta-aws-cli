/*
 * Copyright (c) 2023-Present, Okta, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package m2mauth

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	oaws "github.com/okta/okta-aws-cli/internal/aws"
	"github.com/okta/okta-aws-cli/internal/config"
	"github.com/okta/okta-aws-cli/internal/okta"
	"github.com/okta/okta-aws-cli/internal/output"
	"github.com/okta/okta-aws-cli/internal/utils"
	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

const (
	// DefaultScope The default scope value
	DefaultScope = "okta-m2m-access"
	// DefaultAuthzID The default authorization server id
	DefaultAuthzID = "default"
)

// M2MAuthentication Object structure for headless authentication
type M2MAuthentication struct {
	config *config.Config
}

// NewM2MAuthentication New M2M Authentication constructor
func NewM2MAuthentication(config *config.Config) (*M2MAuthentication, error) {
	// need to set our config defaults
	if config.CustomScope() == "" {
		_ = config.SetCustomScope(DefaultScope)
	}
	if config.AuthzID() == "" {
		_ = config.SetAuthzID(DefaultAuthzID)
	}

	m := M2MAuthentication{
		config: config,
	}
	return &m, nil
}

// EstablishIAMCredentials Full operation to fetch temporary IAM credentials and
// output them to preferred format.
//
// The overall API interactions are as follows:
//
// - CLI requests access token from custom authz server at /oauth2/{authzID}/v1/token
// - CLI presents access token to AWS STS for temporary AWS IAM creds
func (m *M2MAuthentication) EstablishIAMCredentials() error {
	at, err := m.accessToken()
	if err != nil {
		return err
	}

	cred, err := m.awsAssumeRoleWithWebIdentity(at)
	if err != nil {
		return err
	}

	err = output.RenderAWSCredential(m.config, cred)
	if err != nil {
		return err
	}

	return nil
}

func (m *M2MAuthentication) awsAssumeRoleWithWebIdentity(at *okta.AccessToken) (credential *oaws.Credential, err error) {
	awsCfg := aws.NewConfig().WithHTTPClient(m.config.HTTPClient())
	sess, err := session.NewSession(awsCfg)
	if err != nil {
		return nil, err
	}

	svc := sts.New(sess)
	input := &sts.AssumeRoleWithWebIdentityInput{
		DurationSeconds:  aws.Int64(m.config.AWSSessionDuration()),
		RoleArn:          aws.String(m.config.AWSIAMRole()),
		RoleSessionName:  aws.String("okta-aws-cli"),
		WebIdentityToken: &at.AccessToken,
	}
	svcResp, err := svc.AssumeRoleWithWebIdentity(input)
	if err != nil {
		return nil, err
	}

	credential = &oaws.Credential{
		AccessKeyID:     *svcResp.Credentials.AccessKeyId,
		SecretAccessKey: *svcResp.Credentials.SecretAccessKey,
		SessionToken:    *svcResp.Credentials.SessionToken,
	}
	return credential, nil
}

func (m *M2MAuthentication) createKeySigner() (jose.Signer, error) {
	signerOptions := (&jose.SignerOptions{}).WithHeader("kid", m.config.KeyID())
	priv := []byte(strings.ReplaceAll(m.config.PrivateKey(), `\n`, "\n"))

	privPem, _ := pem.Decode(priv)
	if privPem == nil {
		return nil, errors.New("invalid private key")
	}

	if privPem.Type == "RSA PRIVATE KEY" {
		parsedKey, err := x509.ParsePKCS1PrivateKey(privPem.Bytes)
		if err != nil {
			return nil, err
		}
		return jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: parsedKey}, signerOptions)
	}
	if privPem.Type == "PRIVATE KEY" {
		parsedKey, err := x509.ParsePKCS8PrivateKey(privPem.Bytes)
		if err != nil {
			return nil, err
		}
		var alg jose.SignatureAlgorithm
		switch parsedKey.(type) {
		case *rsa.PrivateKey:
			alg = jose.RS256
		case *ecdsa.PrivateKey:
			alg = jose.ES256 // TODO handle ES384 or ES512 ?
		default:
			// TODO are either of these also valid?
			// ed25519.PrivateKey:
			// *ecdh.PrivateKey
			return nil, fmt.Errorf("private key %q is unknown pkcs#8 format type", privPem.Type)
		}
		return jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: parsedKey}, signerOptions)
	}

	return nil, fmt.Errorf("private key %q is not pkcs#1 or pkcs#8 format", privPem.Type)
}

func (m *M2MAuthentication) makeClientAssertion() (string, error) {
	privateKeySinger, err := m.createKeySigner()
	if err != nil {
		return "", err
	}

	tokenRequestURL := fmt.Sprintf(okta.CustomAuthzV1TokenEndpointFormat, m.config.OrgDomain(), m.config.AuthzID())
	now := m.config.Clock().Now()
	claims := okta.ClientAssertionClaims{
		Subject:  m.config.OIDCAppID(),
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour * time.Duration(1))),
		Issuer:   m.config.OIDCAppID(),
		Audience: tokenRequestURL,
	}

	jwtBuilder := jwt.Signed(privateKeySinger).Claims(claims)
	return jwtBuilder.CompactSerialize()
}

// accessToken Takes okta-aws-cli private key and presents a client_credentials
// flow assertion to /oauth2/{authzServerID}/v1/token to gather an access token.
func (m *M2MAuthentication) accessToken() (*okta.AccessToken, error) {
	clientAssertion, err := m.makeClientAssertion()
	if err != nil {
		return nil, err
	}

	var tokenRequestBuff io.ReadWriter
	query := url.Values{}
	tokenRequestURL := fmt.Sprintf(okta.CustomAuthzV1TokenEndpointFormat, m.config.OrgDomain(), m.config.AuthzID())

	query.Add("grant_type", "client_credentials")
	query.Add("scope", m.config.CustomScope())
	query.Add("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	query.Add("client_assertion", clientAssertion)
	tokenRequestURL += "?" + query.Encode()
	req, err := http.NewRequest("POST", tokenRequestURL, tokenRequestBuff)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Accept", utils.ApplicationJSON)
	req.Header.Add(utils.ContentType, utils.ApplicationXFORM)
	req.Header.Add(utils.UserAgentHeader, config.UserAgentValue)
	req.Header.Add(utils.XOktaAWSCLIOperationHeader, utils.XOktaAWSCLIM2MOperation)
	resp, err := m.config.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		baseErrStr := "fetching access token received API response %q"
		if err != nil {
			return nil, fmt.Errorf(baseErrStr, resp.Status)
		}

		var apiErr okta.APIError
		err = json.NewDecoder(resp.Body).Decode(&apiErr)
		if err != nil {
			return nil, fmt.Errorf(baseErrStr, resp.Status)
		}

		return nil, fmt.Errorf(baseErrStr+okta.AccessTokenErrorFormat, resp.Status, apiErr.Error, apiErr.ErrorDescription)
	}

	token := &okta.AccessToken{}
	err = json.NewDecoder(resp.Body).Decode(token)
	if err != nil {
		return nil, err
	}

	return token, nil
}
