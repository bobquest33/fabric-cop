/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package lib

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/cloudflare/cfssl/api"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/log"
	cop "github.com/hyperledger/fabric-cop/api"
	"github.com/hyperledger/fabric-cop/idp"
	"github.com/hyperledger/fabric-cop/util"
)

const (
	// defaultServerPort is the default CFSSL listening port
	defaultServerPort = "8888"
)

// NewClient is the constructor for the COP client API
func NewClient(config string) (*Client, error) {
	c := new(Client)
	// Set defaults
	c.ServerURL = "http://localhost:8888"
	c.HomeDir = util.GetDefaultHomeDir()
	c.MyIDFile = "client.json"
	if config != "" {
		// Override any defaults
		err := util.Unmarshal([]byte(config), c, "NewClient")
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Client is the COP client object
type Client struct {
	// ServerURL is the URL of the server
	ServerURL string `json:"serverURL,omitempty"`
	// HomeDir is the home directory
	HomeDir string `json:"homeDir,omitempty"`
	// MyIDFIle is the name of ID file which gets loaded by LoadMyIdentity
	MyIDFile string `json:"fileName,omitempty"`
}

// Capabilities returns the capabilities COP
func (c *Client) Capabilities() []idp.Capability {
	return []idp.Capability{
		idp.REGISTRATION,
		idp.ENROLLMENT,
		idp.ATTRIBUTES,
		idp.ANONYMITY,
		idp.UNLINKABILITY,
	}
}

// Register registers a new identity
// @param req The registration request
func (c *Client) Register(req *idp.RegistrationRequest) (*idp.RegistrationResponse, error) {
	log.Debugf("Register %+v", req)
	// Send a post to the "register" endpoint with req as body

	if req.Name == "" {
		return nil, errors.New("Register was called without a Name set")
	}
	if req.Group == "" {
		return nil, errors.New("Register was called without a Group set")
	}
	if req.Registrar == nil {
		return nil, errors.New("Register was called without a Registrar identity set")
	}
	var request cop.RegisterRequest
	request.User = req.Name
	request.Type = req.Type
	request.Group = req.Group
	request.Attributes = req.Attributes
	request.CallerID = req.Registrar.GetName()

	newID := newIdentity(c, req.Registrar.GetName(), req.Registrar.(*Identity).GetMyKey(), req.Registrar.(*Identity).GetMyCert())
	req.Registrar = newID

	reqBody, err := util.Marshal(request, "RegistrationRequest")
	if err != nil {
		return nil, err
	}

	buf, err2 := req.Registrar.(*Identity).Post("register", reqBody)
	if err2 != nil {
		return nil, err2
	}

	var response api.Response
	json.Unmarshal(buf, &response)
	resp := new(idp.RegistrationResponse)
	resp.Secret = response.Result.(string)

	log.Debug("The register request completely successfully")
	return resp, nil
}

// Enroll enrolls a new identity
// @param req The enrollment request
func (c *Client) Enroll(req *idp.EnrollmentRequest) (*Identity, error) {
	log.Debugf("Enrolling %+v", req)

	// Generate the CSR
	csrPEM, key, err := c.GenCSR(req.CSR, req.Name)
	if err != nil {
		log.Debugf("enroll failure generating CSR: %s", err)
		return nil, err
	}

	// Send the CSR to the COP server
	post, err := c.NewPost("enroll", csrPEM)
	if err != nil {
		return nil, err
	}
	post.SetBasicAuth(req.Name, req.Secret)
	cert, err := c.SendPost(post)
	if err != nil {
		return nil, err
	}

	// Create an identity from the key and certificate in the response
	return c.newIdentityFromResponse(req.Name, cert, key)
}

// Reenroll reenrolls an existing Identity and returns a new Identity
// @param req The reenrollment request
func (c *Client) Reenroll(req *idp.ReenrollmentRequest) (*Identity, error) {
	log.Debugf("Reenrolling %+v", req)

	if req.ID == nil {
		return nil, errors.New("ReenrollmentRequest.ID was not set")
	}

	id := req.ID.(*Identity)

	csrPEM, key, err := c.GenCSR(req.CSR, id.GetName())
	if err != nil {
		return nil, err
	}

	cert, err := id.Post("reenroll", csrPEM)
	if err != nil {
		return nil, err
	}

	return c.newIdentityFromResponse(id.GetName(), cert, key)
}

// newIdentityFromResponse returns an Identity for enroll and reenroll responses
// @param id Name of identity being enrolled or reenrolled
// @param cert The certificate which was issued
// @param key The private key which was used to sign the request
func (c *Client) newIdentityFromResponse(id string, cert, key []byte) (*Identity, error) {
	log.Debugf("newIdentityFromResponse %s", id)

	var resp api.Response
	err := json.Unmarshal(cert, &resp)
	if err != nil {
		return nil, err
	}

	certByte, _ := base64.StdEncoding.DecodeString(resp.Result.(string))

	if resp.Result != nil && resp.Success == true {
		log.Debugf("newIdentityFromResponse success for %s", id)
		return newIdentity(c, id, key, certByte), nil
	}

	return nil, cop.NewError(cop.EnrollingUserError, "Failed to reenroll user")
}

// RegisterAndEnroll registers and enrolls a new identity
// @param req The registration request
func (c *Client) RegisterAndEnroll(req *idp.RegistrationRequest) (*Identity, error) {
	return nil, errors.New("NotImplemented")
}

// GenCSR generates a CSR (Certificate Signing Request)
func (c *Client) GenCSR(req *idp.CSRInfo, id string) ([]byte, []byte, error) {
	log.Debugf("GenCSR %+v", req)

	cr := c.newCertificateRequest(req)
	cr.CN = id

	csrPEM, key, err := csr.ParseRequest(cr)
	if err != nil {
		log.Debugf("failed generating CSR: %s", err)
		return nil, nil, err
	}

	return csrPEM, key, nil
}

// newCertificateRequest creates a certificate request which is used to generate
// a CSR (Certificate Signing Request)
func (c *Client) newCertificateRequest(req *idp.CSRInfo) *csr.CertificateRequest {
	cr := csr.CertificateRequest{}
	if req != nil && req.Names != nil {
		cr.Names = req.Names
	}
	if req != nil && req.Hosts != nil {
		cr.Hosts = req.Hosts
	} else {
		// Default requested hosts are local hostname
		hostname, _ := os.Hostname()
		if hostname != "" {
			cr.Hosts = make([]string, 1)
			cr.Hosts[0] = hostname
		}
	}
	if req != nil && req.KeyRequest != nil {
		cr.KeyRequest = req.KeyRequest
	}
	if req != nil {
		cr.CA = req.CA
		cr.SerialNumber = req.SerialNumber
	}
	return &cr
}

// ImportSigner imports a signer from an external CA
// @param req The import request
func (c *Client) ImportSigner(req *idp.ImportSignerRequest) (idp.Signer, error) {
	return nil, errors.New("NotImplemented")
}

// LoadMyIdentity loads the client's identity from disk
func (c *Client) LoadMyIdentity() (*Identity, error) {
	myIDFile := c.GetMyIdentityFile()
	if !util.FileExists(myIDFile) {
		return nil, fmt.Errorf("client is not enrolled; '%s' is not an existing file", myIDFile)
	}
	return c.LoadIdentity(myIDFile)
}

// GetMyIdentityFile returns the path to this identity's ID file
func (c *Client) GetMyIdentityFile() string {
	return path.Join(c.HomeDir, c.MyIDFile)
}

// LoadIdentity loads an identity from a file on disk at path
func (c *Client) LoadIdentity(path string) (*Identity, error) {
	buf, err := util.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return c.DeserializeIdentity(buf)
}

// LoadCSRInfo reads CSR (Certificate Signing Request) from a file
// @parameter path The path to the file contains CSR info in JSON format
func (c *Client) LoadCSRInfo(path string) (*idp.CSRInfo, error) {
	csrJSON, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var csrInfo idp.CSRInfo
	err = util.Unmarshal(csrJSON, &csrInfo, "LoadCSRInfo")
	if err != nil {
		return nil, err
	}
	return &csrInfo, nil
}

// DeserializeIdentity deserializes an identity
func (c *Client) DeserializeIdentity(buf []byte) (*Identity, error) {
	id := new(Identity)
	err := util.Unmarshal(buf, id, "Identity")
	id.client = c
	return id, err
}

// NewPost create a new post request
func (c *Client) NewPost(endpoint string, reqBody []byte) (*http.Request, error) {
	curl, cerr := c.getURL(endpoint)
	if cerr != nil {
		return nil, cerr
	}
	req, err := http.NewRequest("POST", curl, bytes.NewReader(reqBody))
	if err != nil {
		msg := fmt.Sprintf("failed to create new request to %s: %v", curl, err)
		log.Debug(msg)
		return nil, cop.NewError(cop.CFSSL, msg)
	}
	return req, nil
}

// SendPost sends a request to the LDAP server and returns a response
func (c *Client) SendPost(req *http.Request) (respBody []byte, err error) {
	log.Debugf("Sending request\n%s", util.HTTPRequestToString(req))
	req.Header.Set("content-type", "application/json")
	httpClient := &http.Client{}
	// TODO: Add TLS
	resp, err := httpClient.Do(req)
	if err != nil {
		msg := fmt.Sprintf("POST failed: %v", err)
		log.Debug(msg)
		return nil, errors.New(msg)
	}
	if resp.Body != nil {
		respBody, err = ioutil.ReadAll(resp.Body)
		defer resp.Body.Close()
		if err != nil {
			msg := fmt.Sprintf("failed to read response: %v", err)
			log.Debug(msg)
			return nil, errors.New(msg)
		}
		log.Debugf("Received response\n%s", util.HTTPResponseToString(resp))
	}
	if resp.StatusCode >= 400 {
		var msg string
		body := new(api.Response)
		err = json.Unmarshal(respBody, body)
		if err != nil && len(body.Errors) > 0 {
			msg = body.Errors[0].Message
		} else {
			msg = fmt.Sprintf("HTTP status code=%d; %s", resp.StatusCode, string(respBody))
		}
		msg = fmt.Sprintf("Error response from COP server; %s", msg)
		log.Debugf("%s", msg)
		return nil, errors.New(msg)
	}
	return respBody, nil
}

func (c *Client) getURL(endpoint string) (string, cop.Error) {
	nurl, err := normalizeURL(c.ServerURL)
	if err != nil {
		log.Debugf("error getting server URL: %s", err)
		return "", cop.WrapError(err, cop.CFSSL, "error getting URL for %s", endpoint)
	}
	rtn := fmt.Sprintf("%s/api/v1/cfssl/%s", nurl, endpoint)
	return rtn, nil
}

func normalizeURL(addr string) (*url.URL, error) {
	addr = strings.TrimSpace(addr)
	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	if u.Opaque != "" {
		u.Host = net.JoinHostPort(u.Scheme, u.Opaque)
		u.Opaque = ""
	} else if u.Path != "" && !strings.Contains(u.Path, ":") {
		u.Host = net.JoinHostPort(u.Path, defaultServerPort)
		u.Path = ""
	} else if u.Scheme == "" {
		u.Host = u.Path
		u.Path = ""
	}
	if u.Scheme != "https" {
		u.Scheme = "http"
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		_, port, err = net.SplitHostPort(u.Host + ":" + defaultServerPort)
		if err != nil {
			return nil, err
		}
	}
	if port != "" {
		_, err = strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
	}
	return u, nil
}
