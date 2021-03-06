// Package acme implements the ACME protocol for Let's Encrypt and other conforming providers.
package acme

import (
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xenolf/lego/log"
)

const (
	// maxBodySize is the maximum size of body that we will read.
	maxBodySize = 1024 * 1024

	// overallRequestLimit is the overall number of request per second limited on the
	// “new-reg”, “new-authz” and “new-cert” endpoints. From the documentation the
	// limitation is 20 requests per second, but using 20 as value doesn't work but 18 do
	overallRequestLimit = 18

	statusValid   = "valid"
	statusInvalid = "invalid"
)

// User interface is to be implemented by users of this library.
// It is used by the client type to get user specific information.
type User interface {
	GetEmail() string
	GetRegistration() *RegistrationResource
	GetPrivateKey() crypto.PrivateKey
}

// Interface for all challenge solvers to implement.
type solver interface {
	Solve(challenge challenge, domain string) error
}

// Interface for challenges like dns, where we can set a record in advance for ALL challenges.
// This saves quite a bit of time vs creating the records and solving them serially.
type preSolver interface {
	PreSolve(challenge challenge, domain string) error
}

// Interface for challenges like dns, where we can solve all the challenges before to delete them.
type cleanup interface {
	CleanUp(challenge challenge, domain string) error
}

type validateFunc func(j *jws, domain, uri string, chlng challenge) error

// Client is the user-friendy way to ACME
type Client struct {
	directory directory
	user      User
	jws       *jws
	keyType   KeyType
	solvers   map[Challenge]solver
}

// NewClient creates a new ACME client on behalf of the user. The client will depend on
// the ACME directory located at caDirURL for the rest of its actions.  A private
// key of type keyType (see KeyType contants) will be generated when requesting a new
// certificate if one isn't provided.
func NewClient(caDirURL string, user User, keyType KeyType) (*Client, error) {
	privKey := user.GetPrivateKey()
	if privKey == nil {
		return nil, errors.New("private key was nil")
	}

	var dir directory
	if _, err := getJSON(caDirURL, &dir); err != nil {
		return nil, fmt.Errorf("get directory at '%s': %v", caDirURL, err)
	}

	if dir.NewAccountURL == "" {
		return nil, errors.New("directory missing new registration URL")
	}
	if dir.NewOrderURL == "" {
		return nil, errors.New("directory missing new order URL")
	}

	jws := &jws{privKey: privKey, getNonceURL: dir.NewNonceURL}
	if reg := user.GetRegistration(); reg != nil {
		jws.kid = reg.URI
	}

	// REVIEW: best possibility?
	// Add all available solvers with the right index as per ACME
	// spec to this map. Otherwise they won`t be found.
	solvers := map[Challenge]solver{
		HTTP01:    &httpChallenge{jws: jws, validate: validate, provider: &HTTPProviderServer{}},
		TLSALPN01: &tlsALPNChallenge{jws: jws, validate: validate, provider: &TLSALPNProviderServer{}},
	}

	return &Client{directory: dir, user: user, jws: jws, keyType: keyType, solvers: solvers}, nil
}

// SetChallengeProvider specifies a custom provider p that can solve the given challenge type.
func (c *Client) SetChallengeProvider(challenge Challenge, p ChallengeProvider) error {
	switch challenge {
	case HTTP01:
		c.solvers[challenge] = &httpChallenge{jws: c.jws, validate: validate, provider: p}
	case DNS01:
		c.solvers[challenge] = &dnsChallenge{jws: c.jws, validate: validate, provider: p}
	case TLSALPN01:
		c.solvers[challenge] = &tlsALPNChallenge{jws: c.jws, validate: validate, provider: p}
	default:
		return fmt.Errorf("unknown challenge %v", challenge)
	}
	return nil
}

// SetHTTPAddress specifies a custom interface:port to be used for HTTP based challenges.
// If this option is not used, the default port 80 and all interfaces will be used.
// To only specify a port and no interface use the ":port" notation.
//
// NOTE: This REPLACES any custom HTTP provider previously set by calling
// c.SetChallengeProvider with the default HTTP challenge provider.
func (c *Client) SetHTTPAddress(iface string) error {
	host, port, err := net.SplitHostPort(iface)
	if err != nil {
		return err
	}

	if chlng, ok := c.solvers[HTTP01]; ok {
		chlng.(*httpChallenge).provider = NewHTTPProviderServer(host, port)
	}

	return nil
}

// SetTLSAddress specifies a custom interface:port to be used for TLS based challenges.
// If this option is not used, the default port 443 and all interfaces will be used.
// To only specify a port and no interface use the ":port" notation.
//
// NOTE: This REPLACES any custom TLS-ALPN provider previously set by calling
// c.SetChallengeProvider with the default TLS-ALPN challenge provider.
func (c *Client) SetTLSAddress(iface string) error {
	host, port, err := net.SplitHostPort(iface)
	if err != nil {
		return err
	}

	if chlng, ok := c.solvers[TLSALPN01]; ok {
		chlng.(*tlsALPNChallenge).provider = NewTLSALPNProviderServer(host, port)
	}
	return nil
}

// ExcludeChallenges explicitly removes challenges from the pool for solving.
func (c *Client) ExcludeChallenges(challenges []Challenge) {
	// Loop through all challenges and delete the requested one if found.
	for _, challenge := range challenges {
		delete(c.solvers, challenge)
	}
}

// GetToSURL returns the current ToS URL from the Directory
func (c *Client) GetToSURL() string {
	return c.directory.Meta.TermsOfService
}

// GetExternalAccountRequired returns the External Account Binding requirement of the Directory
func (c *Client) GetExternalAccountRequired() bool {
	return c.directory.Meta.ExternalAccountRequired
}

// Register the current account to the ACME server.
func (c *Client) Register(tosAgreed bool) (*RegistrationResource, error) {
	if c == nil || c.user == nil {
		return nil, errors.New("acme: cannot register a nil client or user")
	}
	log.Infof("acme: Registering account for %s", c.user.GetEmail())

	accMsg := accountMessage{}
	if c.user.GetEmail() != "" {
		accMsg.Contact = []string{"mailto:" + c.user.GetEmail()}
	} else {
		accMsg.Contact = []string{}
	}
	accMsg.TermsOfServiceAgreed = tosAgreed

	var serverReg accountMessage
	hdr, err := postJSON(c.jws, c.directory.NewAccountURL, accMsg, &serverReg)
	if err != nil {
		remoteErr, ok := err.(RemoteError)
		if ok && remoteErr.StatusCode == 409 {
		} else {
			return nil, err
		}
	}

	reg := &RegistrationResource{
		URI:  hdr.Get("Location"),
		Body: serverReg,
	}
	c.jws.kid = reg.URI

	return reg, nil
}

// RegisterWithExternalAccountBinding Register the current account to the ACME server.
func (c *Client) RegisterWithExternalAccountBinding(tosAgreed bool, kid string, hmacEncoded string) (*RegistrationResource, error) {
	if c == nil || c.user == nil {
		return nil, errors.New("acme: cannot register a nil client or user")
	}
	log.Infof("acme: Registering account (EAB) for %s", c.user.GetEmail())

	accMsg := accountMessage{}
	if c.user.GetEmail() != "" {
		accMsg.Contact = []string{"mailto:" + c.user.GetEmail()}
	} else {
		accMsg.Contact = []string{}
	}
	accMsg.TermsOfServiceAgreed = tosAgreed

	hmac, err := base64.RawURLEncoding.DecodeString(hmacEncoded)
	if err != nil {
		return nil, fmt.Errorf("acme: could not decode hmac key: %s", err.Error())
	}

	eabJWS, err := c.jws.signEABContent(c.directory.NewAccountURL, kid, hmac)
	if err != nil {
		return nil, fmt.Errorf("acme: error signing eab content: %s", err.Error())
	}

	eabPayload := eabJWS.FullSerialize()

	accMsg.ExternalAccountBinding = []byte(eabPayload)

	var serverReg accountMessage
	hdr, err := postJSON(c.jws, c.directory.NewAccountURL, accMsg, &serverReg)
	if err != nil {
		remoteErr, ok := err.(RemoteError)
		if ok && remoteErr.StatusCode == 409 {
		} else {
			return nil, err
		}
	}

	reg := &RegistrationResource{
		URI:  hdr.Get("Location"),
		Body: serverReg,
	}
	c.jws.kid = reg.URI

	return reg, nil
}

// ResolveAccountByKey will attempt to look up an account using the given account key
// and return its registration resource.
func (c *Client) ResolveAccountByKey() (*RegistrationResource, error) {
	log.Infof("acme: Trying to resolve account by key")

	acc := accountMessage{OnlyReturnExisting: true}
	hdr, err := postJSON(c.jws, c.directory.NewAccountURL, acc, nil)
	if err != nil {
		return nil, err
	}

	accountLink := hdr.Get("Location")
	if accountLink == "" {
		return nil, errors.New("Server did not return the account link")
	}

	var retAccount accountMessage
	c.jws.kid = accountLink
	_, err = postJSON(c.jws, accountLink, accountMessage{}, &retAccount)
	if err != nil {
		return nil, err
	}

	return &RegistrationResource{URI: accountLink, Body: retAccount}, nil
}

// DeleteRegistration deletes the client's user registration from the ACME
// server.
func (c *Client) DeleteRegistration() error {
	if c == nil || c.user == nil {
		return errors.New("acme: cannot unregister a nil client or user")
	}
	log.Infof("acme: Deleting account for %s", c.user.GetEmail())

	accMsg := accountMessage{
		Status: "deactivated",
	}

	_, err := postJSON(c.jws, c.user.GetRegistration().URI, accMsg, nil)
	return err
}

// QueryRegistration runs a POST request on the client's registration and
// returns the result.
//
// This is similar to the Register function, but acting on an existing
// registration link and resource.
func (c *Client) QueryRegistration() (*RegistrationResource, error) {
	if c == nil || c.user == nil {
		return nil, errors.New("acme: cannot query the registration of a nil client or user")
	}
	// Log the URL here instead of the email as the email may not be set
	log.Infof("acme: Querying account for %s", c.user.GetRegistration().URI)

	accMsg := accountMessage{}

	var serverReg accountMessage
	_, err := postJSON(c.jws, c.user.GetRegistration().URI, accMsg, &serverReg)
	if err != nil {
		return nil, err
	}

	reg := &RegistrationResource{Body: serverReg}

	// Location: header is not returned so this needs to be populated off of
	// existing URI
	reg.URI = c.user.GetRegistration().URI

	return reg, nil
}

// ObtainCertificateForCSR tries to obtain a certificate matching the CSR passed into it.
// The domains are inferred from the CommonName and SubjectAltNames, if any. The private key
// for this CSR is not required.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// This function will never return a partial certificate. If one domain in the list fails,
// the whole certificate will fail.
func (c *Client) ObtainCertificateForCSR(csr x509.CertificateRequest, bundle bool) (*CertificateResource, error) {
	// figure out what domains it concerns
	// start with the common name
	domains := []string{csr.Subject.CommonName}

	// loop over the SubjectAltName DNS names
DNSNames:
	for _, sanName := range csr.DNSNames {
		for _, existingName := range domains {
			if existingName == sanName {
				// duplicate; skip this name
				continue DNSNames
			}
		}

		// name is unique
		domains = append(domains, sanName)
	}

	if bundle {
		log.Infof("[%s] acme: Obtaining bundled SAN certificate given a CSR", strings.Join(domains, ", "))
	} else {
		log.Infof("[%s] acme: Obtaining SAN certificate given a CSR", strings.Join(domains, ", "))
	}

	order, err := c.createOrderForIdentifiers(domains)
	if err != nil {
		return nil, err
	}
	authz, err := c.getAuthzForOrder(order)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		/*for _, auth := range authz {
			c.disableAuthz(auth)
		}*/
		return nil, err
	}

	err = c.solveChallengeForAuthz(authz)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		return nil, err
	}

	log.Infof("[%s] acme: Validations succeeded; requesting certificates", strings.Join(domains, ", "))

	failures := make(ObtainError)
	cert, err := c.requestCertificateForCsr(order, bundle, csr.Raw, nil)
	if err != nil {
		for _, chln := range authz {
			failures[chln.Identifier.Value] = err
		}
	}

	if cert != nil {
		// Add the CSR to the certificate so that it can be used for renewals.
		cert.CSR = pemEncode(&csr)
	}

	// do not return an empty failures map, because
	// it would still be a non-nil error value
	if len(failures) > 0 {
		return cert, failures
	}
	return cert, nil
}

// ObtainCertificate tries to obtain a single certificate using all domains passed into it.
// The first domain in domains is used for the CommonName field of the certificate, all other
// domains are added using the Subject Alternate Names extension. A new private key is generated
// for every invocation of this function. If you do not want that you can supply your own private key
// in the privKey parameter. If this parameter is non-nil it will be used instead of generating a new one.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// This function will never return a partial certificate. If one domain in the list fails,
// the whole certificate will fail.
func (c *Client) ObtainCertificate(domains []string, bundle bool, privKey crypto.PrivateKey, mustStaple bool) (*CertificateResource, error) {
	if len(domains) == 0 {
		return nil, errors.New("no domains to obtain a certificate for")
	}

	if bundle {
		log.Infof("[%s] acme: Obtaining bundled SAN certificate", strings.Join(domains, ", "))
	} else {
		log.Infof("[%s] acme: Obtaining SAN certificate", strings.Join(domains, ", "))
	}

	order, err := c.createOrderForIdentifiers(domains)
	if err != nil {
		return nil, err
	}
	authz, err := c.getAuthzForOrder(order)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		/*for _, auth := range authz {
			c.disableAuthz(auth)
		}*/
		return nil, err
	}

	err = c.solveChallengeForAuthz(authz)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		return nil, err
	}

	log.Infof("[%s] acme: Validations succeeded; requesting certificates", strings.Join(domains, ", "))

	failures := make(ObtainError)
	cert, err := c.requestCertificateForOrder(order, bundle, privKey, mustStaple)
	if err != nil {
		for _, auth := range authz {
			failures[auth.Identifier.Value] = err
		}
	}

	// do not return an empty failures map, because
	// it would still be a non-nil error value
	if len(failures) > 0 {
		return cert, failures
	}
	return cert, nil
}

// RevokeCertificate takes a PEM encoded certificate or bundle and tries to revoke it at the CA.
func (c *Client) RevokeCertificate(certificate []byte) error {
	certificates, err := parsePEMBundle(certificate)
	if err != nil {
		return err
	}

	x509Cert := certificates[0]
	if x509Cert.IsCA {
		return fmt.Errorf("Certificate bundle starts with a CA certificate")
	}

	encodedCert := base64.URLEncoding.EncodeToString(x509Cert.Raw)

	_, err = postJSON(c.jws, c.directory.RevokeCertURL, revokeCertMessage{Certificate: encodedCert}, nil)
	return err
}

// RenewCertificate takes a CertificateResource and tries to renew the certificate.
// If the renewal process succeeds, the new certificate will ge returned in a new CertResource.
// Please be aware that this function will return a new certificate in ANY case that is not an error.
// If the server does not provide us with a new cert on a GET request to the CertURL
// this function will start a new-cert flow where a new certificate gets generated.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// For private key reuse the PrivateKey property of the passed in CertificateResource should be non-nil.
func (c *Client) RenewCertificate(cert CertificateResource, bundle, mustStaple bool) (*CertificateResource, error) {
	// Input certificate is PEM encoded. Decode it here as we may need the decoded
	// cert later on in the renewal process. The input may be a bundle or a single certificate.
	certificates, err := parsePEMBundle(cert.Certificate)
	if err != nil {
		return nil, err
	}

	x509Cert := certificates[0]
	if x509Cert.IsCA {
		return nil, fmt.Errorf("[%s] Certificate bundle starts with a CA certificate", cert.Domain)
	}

	// This is just meant to be informal for the user.
	timeLeft := x509Cert.NotAfter.Sub(time.Now().UTC())
	log.Infof("[%s] acme: Trying renewal with %d hours remaining", cert.Domain, int(timeLeft.Hours()))

	// We always need to request a new certificate to renew.
	// Start by checking to see if the certificate was based off a CSR, and
	// use that if it's defined.
	if len(cert.CSR) > 0 {
		csr, errP := pemDecodeTox509CSR(cert.CSR)
		if errP != nil {
			return nil, errP
		}
		newCert, failures := c.ObtainCertificateForCSR(*csr, bundle)
		return newCert, failures
	}

	var privKey crypto.PrivateKey
	if cert.PrivateKey != nil {
		privKey, err = parsePEMPrivateKey(cert.PrivateKey)
		if err != nil {
			return nil, err
		}
	}

	var domains []string
	// check for SAN certificate
	if len(x509Cert.DNSNames) > 1 {
		domains = append(domains, x509Cert.Subject.CommonName)
		for _, sanDomain := range x509Cert.DNSNames {
			if sanDomain == x509Cert.Subject.CommonName {
				continue
			}
			domains = append(domains, sanDomain)
		}
	} else {
		domains = append(domains, x509Cert.Subject.CommonName)
	}

	newCert, err := c.ObtainCertificate(domains, bundle, privKey, mustStaple)
	return newCert, err
}

func (c *Client) createOrderForIdentifiers(domains []string) (orderResource, error) {
	var identifiers []identifier
	for _, domain := range domains {
		identifiers = append(identifiers, identifier{Type: "dns", Value: domain})
	}

	order := orderMessage{
		Identifiers: identifiers,
	}

	var response orderMessage
	hdr, err := postJSON(c.jws, c.directory.NewOrderURL, order, &response)
	if err != nil {
		return orderResource{}, err
	}

	orderRes := orderResource{
		URL:          hdr.Get("Location"),
		Domains:      domains,
		orderMessage: response,
	}
	return orderRes, nil
}

// an authz with the solver we have chosen and the index of the challenge associated with it
type selectedAuthSolver struct {
	authz          authorization
	challengeIndex int
	solver         solver
}

// Looks through the challenge combinations to find a solvable match.
// Then solves the challenges in series and returns.
func (c *Client) solveChallengeForAuthz(authorizations []authorization) error {
	failures := make(ObtainError)

	authSolvers := []*selectedAuthSolver{}

	// loop through the resources, basically through the domains. First pass just selects a solver for each authz.
	for _, authz := range authorizations {
		if authz.Status == statusValid {
			// Boulder might recycle recent validated authz (see issue #267)
			log.Infof("[%s] acme: Authorization already valid; skipping challenge", authz.Identifier.Value)
			continue
		}
		if i, solvr := c.chooseSolver(authz, authz.Identifier.Value); solvr != nil {
			authSolvers = append(authSolvers, &selectedAuthSolver{
				authz:          authz,
				challengeIndex: i,
				solver:         solvr,
			})
		} else {
			failures[authz.Identifier.Value] = fmt.Errorf("[%s] acme: Could not determine solvers", authz.Identifier.Value)
		}
	}

	// for all valid presolvers, first submit the challenges so they have max time to propagate
	for _, item := range authSolvers {
		authz := item.authz
		i := item.challengeIndex
		if presolver, ok := item.solver.(preSolver); ok {
			if err := presolver.PreSolve(authz.Challenges[i], authz.Identifier.Value); err != nil {
				failures[authz.Identifier.Value] = err
			}
		}
	}

	defer func() {
		// clean all created TXT records
		for _, item := range authSolvers {
			if clean, ok := item.solver.(cleanup); ok {
				if failures[item.authz.Identifier.Value] != nil {
					// already failed in previous loop
					continue
				}
				err := clean.CleanUp(item.authz.Challenges[item.challengeIndex], item.authz.Identifier.Value)
				if err != nil {
					log.Warnf("Error cleaning up %s: %v ", item.authz.Identifier.Value, err)
				}
			}
		}
	}()

	// finally solve all challenges for real
	for _, item := range authSolvers {
		authz := item.authz
		i := item.challengeIndex
		if failures[authz.Identifier.Value] != nil {
			// already failed in previous loop
			continue
		}
		if err := item.solver.Solve(authz.Challenges[i], authz.Identifier.Value); err != nil {
			failures[authz.Identifier.Value] = err
		}
	}

	// be careful not to return an empty failures map, for
	// even an empty ObtainError is a non-nil error value
	if len(failures) > 0 {
		return failures
	}
	return nil
}

// Checks all challenges from the server in order and returns the first matching solver.
func (c *Client) chooseSolver(auth authorization, domain string) (int, solver) {
	for i, challenge := range auth.Challenges {
		if solver, ok := c.solvers[Challenge(challenge.Type)]; ok {
			return i, solver
		}
		log.Infof("[%s] acme: Could not find solver for: %s", domain, challenge.Type)
	}
	return 0, nil
}

// Get the challenges needed to proof our identifier to the ACME server.
func (c *Client) getAuthzForOrder(order orderResource) ([]authorization, error) {
	resc, errc := make(chan authorization), make(chan domainError)

	delay := time.Second / overallRequestLimit

	for _, authzURL := range order.Authorizations {
		time.Sleep(delay)

		go func(authzURL string) {
			var authz authorization
			_, err := postAsGet(c.jws, authzURL, &authz)
			if err != nil {
				errc <- domainError{Domain: authz.Identifier.Value, Error: err}
				return
			}

			resc <- authz
		}(authzURL)
	}

	var responses []authorization
	failures := make(ObtainError)
	for i := 0; i < len(order.Authorizations); i++ {
		select {
		case res := <-resc:
			responses = append(responses, res)
		case err := <-errc:
			failures[err.Domain] = err.Error
		}
	}

	logAuthz(order)

	close(resc)
	close(errc)

	// be careful to not return an empty failures map;
	// even if empty, they become non-nil error values
	if len(failures) > 0 {
		return responses, failures
	}
	return responses, nil
}

func logAuthz(order orderResource) {
	for i, auth := range order.Authorizations {
		log.Infof("[%s] AuthURL: %s", order.Identifiers[i].Value, auth)
	}
}

// cleanAuthz loops through the passed in slice and disables any auths which are not "valid"
func (c *Client) disableAuthz(authURL string) error {
	var disabledAuth authorization
	_, err := postJSON(c.jws, authURL, deactivateAuthMessage{Status: "deactivated"}, &disabledAuth)
	return err
}

func (c *Client) requestCertificateForOrder(order orderResource, bundle bool, privKey crypto.PrivateKey, mustStaple bool) (*CertificateResource, error) {

	var err error
	if privKey == nil {
		privKey, err = generatePrivateKey(c.keyType)
		if err != nil {
			return nil, err
		}
	}

	// determine certificate name(s) based on the authorization resources
	commonName := order.Domains[0]

	// ACME draft Section 7.4 "Applying for Certificate Issuance"
	// https://tools.ietf.org/html/draft-ietf-acme-acme-12#section-7.4
	// says:
	//   Clients SHOULD NOT make any assumptions about the sort order of
	//   "identifiers" or "authorizations" elements in the returned order
	//   object.
	san := []string{commonName}
	for _, auth := range order.Identifiers {
		if auth.Value != commonName {
			san = append(san, auth.Value)
		}
	}

	// TODO: should the CSR be customizable?
	csr, err := generateCsr(privKey, commonName, san, mustStaple)
	if err != nil {
		return nil, err
	}

	return c.requestCertificateForCsr(order, bundle, csr, pemEncode(privKey))
}

func (c *Client) requestCertificateForCsr(order orderResource, bundle bool, csr []byte, privateKeyPem []byte) (*CertificateResource, error) {
	commonName := order.Domains[0]

	csrString := base64.RawURLEncoding.EncodeToString(csr)
	var retOrder orderMessage
	_, err := postJSON(c.jws, order.Finalize, csrMessage{Csr: csrString}, &retOrder)
	if err != nil {
		return nil, err
	}

	if retOrder.Status == statusInvalid {
		return nil, err
	}

	certRes := CertificateResource{
		Domain:     commonName,
		CertURL:    retOrder.Certificate,
		PrivateKey: privateKeyPem,
	}

	if retOrder.Status == statusValid {
		// if the certificate is available right away, short cut!
		ok, err := c.checkCertResponse(retOrder, &certRes, bundle)
		if err != nil {
			return nil, err
		}

		if ok {
			return &certRes, nil
		}
	}

	stopTimer := time.NewTimer(30 * time.Second)
	defer stopTimer.Stop()
	retryTick := time.NewTicker(500 * time.Millisecond)
	defer retryTick.Stop()

	for {
		select {
		case <-stopTimer.C:
			return nil, errors.New("certificate polling timed out")
		case <-retryTick.C:
			_, err := postAsGet(c.jws, order.URL, &retOrder)
			if err != nil {
				return nil, err
			}

			done, err := c.checkCertResponse(retOrder, &certRes, bundle)
			if err != nil {
				return nil, err
			}
			if done {
				return &certRes, nil
			}
		}
	}
}

// checkCertResponse checks to see if the certificate is ready and a link is contained in the
// response. if so, loads it into certRes and returns true. If the cert
// is not yet ready, it returns false. The certRes input
// should already have the Domain (common name) field populated. If bundle is
// true, the certificate will be bundled with the issuer's cert.
func (c *Client) checkCertResponse(order orderMessage, certRes *CertificateResource, bundle bool) (bool, error) {
	switch order.Status {
	case statusValid:
		resp, err := postAsGet(c.jws, order.Certificate, nil)
		if err != nil {
			return false, err
		}

		cert, err := ioutil.ReadAll(limitReader(resp.Body, maxBodySize))
		if err != nil {
			return false, err
		}

		// The issuer certificate link may be supplied via an "up" link
		// in the response headers of a new certificate.  See
		// https://tools.ietf.org/html/draft-ietf-acme-acme-12#section-7.4.2
		links := parseLinks(resp.Header["Link"])
		if link, ok := links["up"]; ok {
			issuerCert, err := c.getIssuerCertificate(link)

			if err != nil {
				// If we fail to acquire the issuer cert, return the issued certificate - do not fail.
				log.Warnf("[%s] acme: Could not bundle issuer certificate: %v", certRes.Domain, err)
			} else {
				issuerCert = pemEncode(derCertificateBytes(issuerCert))

				// If bundle is true, we want to return a certificate bundle.
				// To do this, we append the issuer cert to the issued cert.
				if bundle {
					cert = append(cert, issuerCert...)
				}

				certRes.IssuerCertificate = issuerCert
			}
		} else {
			// Get issuerCert from bundled response from Let's Encrypt
			// See https://community.letsencrypt.org/t/acme-v2-no-up-link-in-response/64962
			_, rest := pem.Decode(cert)
			if rest != nil {
				certRes.IssuerCertificate = rest
			}
		}

		certRes.Certificate = cert
		certRes.CertURL = order.Certificate
		certRes.CertStableURL = order.Certificate
		log.Infof("[%s] Server responded with a certificate.", certRes.Domain)
		return true, nil

	case "processing":
		return false, nil
	case statusInvalid:
		return false, errors.New("order has invalid state: invalid")
	default:
		return false, nil
	}
}

// getIssuerCertificate requests the issuer certificate
func (c *Client) getIssuerCertificate(url string) ([]byte, error) {
	log.Infof("acme: Requesting issuer cert from %s", url)
	resp, err := postAsGet(c.jws, url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	issuerBytes, err := ioutil.ReadAll(limitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, err
	}

	_, err = x509.ParseCertificate(issuerBytes)
	if err != nil {
		return nil, err
	}

	return issuerBytes, err
}

func parseLinks(links []string) map[string]string {
	aBrkt := regexp.MustCompile("[<>]")
	slver := regexp.MustCompile("(.+) *= *\"(.+)\"")
	linkMap := make(map[string]string)

	for _, link := range links {

		link = aBrkt.ReplaceAllString(link, "")
		parts := strings.Split(link, ";")

		matches := slver.FindStringSubmatch(parts[1])
		if len(matches) > 0 {
			linkMap[matches[2]] = parts[0]
		}
	}

	return linkMap
}

// validate makes the ACME server start validating a
// challenge response, only returning once it is done.
func validate(j *jws, domain, uri string, c challenge) error {
	var chlng challenge

	// Challenge initiation is done by sending a JWS payload containing the
	// trivial JSON object `{}`. We use an empty struct instance as the postJSON
	// payload here to achieve this result.
	hdr, err := postJSON(j, uri, struct{}{}, &chlng)
	if err != nil {
		return err
	}

	// After the path is sent, the ACME server will access our server.
	// Repeatedly check the server for an updated status on our request.
	for {
		switch chlng.Status {
		case statusValid:
			log.Infof("[%s] The server validated our request", domain)
			return nil
		case "pending":
		case "processing":
		case statusInvalid:
			return handleChallengeError(chlng)
		default:
			return errors.New("the server returned an unexpected state")
		}

		ra, err := strconv.Atoi(hdr.Get("Retry-After"))
		if err != nil {
			// The ACME server MUST return a Retry-After.
			// If it doesn't, we'll just poll hard.
			ra = 5
		}

		time.Sleep(time.Duration(ra) * time.Second)

		resp, err := postAsGet(j, uri, &chlng)
		if err != nil {
			return err
		}
		if resp != nil {
			hdr = resp.Header
		}
	}
}
