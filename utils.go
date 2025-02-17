package main

// utils module
//
// Copyright (c) 2020 - Valentin Kuznetsov <vkuznet@gmail.com>
//

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/vkuznet/auth-proxy-server/cric"
	"golang.org/x/crypto/acme/autocert"
)

// helper function to check if given file name exists
func checkFile(fname string) string {
	_, err := os.Stat(fname)
	if err == nil {
		return fname
	}
	log.Fatalf("unable to read %s, error %v\n", fname, err)
	return ""
}

// helper function to print JSON data
func printJSON(j interface{}, msg string) error {
	if msg != "" {
		log.Println(msg)
	}
	var out []byte
	var err error
	out, err = json.MarshalIndent(j, "", "    ")
	if err == nil {
		log.Println(string(out))
	}
	return err
}

// helper function to print HTTP request information
func printHTTPRequest(r *http.Request, msg string) {
	log.Printf("HTTP request: %s\n", msg)
	log.Println("TLS:", r.TLS)
	log.Println("Header:", r.Header)

	// print out all request headers
	log.Printf("%s %s %s \n", r.Method, r.URL, r.Proto)
	for k, v := range r.Header {
		log.Printf("Header field %q, Value %q\n", k, v)
	}
	log.Printf("Host = %q\n", r.Host)
	log.Printf("RemoteAddr= %q\n", r.RemoteAddr)
	log.Printf("\n\nFinding value of \"Accept\" %q\n", r.Header["Accept"])
}

// RootCAs returns cert pool of our root CAs
func RootCAs() *x509.CertPool {
	log.Println("Load RootCAs from", Config.RootCAs)
	rootCAs := x509.NewCertPool()
	files, err := ioutil.ReadDir(Config.RootCAs)
	if err != nil {
		log.Printf("Unable to list files in '%s', error: %v\n", Config.RootCAs, err)
		return rootCAs
	}
	for _, finfo := range files {
		fname := fmt.Sprintf("%s/%s", Config.RootCAs, finfo.Name())
		caCert, err := os.ReadFile(filepath.Clean(fname))
		if err != nil {
			if Config.Verbose > 1 {
				log.Printf("Unable to read %s\n", fname)
			}
		}
		if ok := rootCAs.AppendCertsFromPEM(caCert); !ok {
			if Config.Verbose > 1 {
				log.Printf("invalid PEM format while importing trust-chain: %q", fname)
			}
		}
		if Config.Verbose > 1 {
			log.Println("Load CA file", fname)
		}
	}
	return rootCAs
}

// global rootCAs
var _rootCAs *x509.CertPool

// VerifyPeerCertificate function provides custom verification of client's
// certificate, see details
// https://golang.org/pkg/crypto/tls/#example_Config_verifyPeerCertificate
// https://www.example-code.com/golang/cert.asp
// https://golang.org/pkg/crypto/x509/pkix/#Extension
func VerifyPeerCertificate(certificates [][]byte, _ [][]*x509.Certificate) error {
	if Config.Verbose > 1 {
		log.Println("call custom tlsConfig.VerifyPeerCertificate")
	}
	certs := make([]*x509.Certificate, len(certificates))
	for i, asn1Data := range certificates {
		cert, err := x509.ParseCertificate(asn1Data)
		if err != nil {
			return errors.New("tls: failed to parse certificate from server: " + err.Error())
		}
		if Config.Verbose > 1 {
			log.Println("Issuer", cert.Issuer)
			log.Println("Subject", cert.Subject)
			log.Println("emails", cert.EmailAddresses)
		}
		// check validity of user certificate
		tstamp := time.Now().Unix()
		if cert.NotBefore.Unix() > tstamp || cert.NotAfter.Unix() < tstamp {
			msg := fmt.Sprintf("Expired user certificate, valid from %v to %v\n", cert.NotBefore, cert.NotAfter)
			return errors.New(msg)
		}
		// dump cert UnhandledCriticalExtensions
		for _, ext := range cert.UnhandledCriticalExtensions {
			if Config.Verbose > 1 {
				log.Printf("Cetificate extension: %+v\n", ext)
			}
			continue
		}
		if len(cert.UnhandledCriticalExtensions) == 0 && cert != nil {
			certs[i] = cert
		}
	}
	if Config.Verbose > 1 {
		log.Println("### number of certs", len(certs))
		for _, cert := range certs {
			if cert != nil {
				log.Printf("issuer %v subject %v valid from %v till %v\n", cert.Issuer, cert.Subject, cert.NotBefore, cert.NotAfter)
			}
		}
	}
	opts := x509.VerifyOptions{
		Roots:         _rootCAs,
		Intermediates: x509.NewCertPool(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if len(certs) > 0 && certs[0] != nil {
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, err := certs[0].Verify(opts)
		return err
	}
	for _, cert := range certs {
		if cert == nil {
			continue
		}
		_, err := cert.Verify(opts)
		if err != nil {
			return err
		}
	}
	return nil
}

// helper function to construct http server with TLS
func getServer(serverCrt, serverKey string, customVerify bool) (*http.Server, error) {
	// start HTTP or HTTPs server based on provided configuration

	var tlsConfig *tls.Config
	// see go doc tls.VersionTLS13 for different versions
	var minVer, maxVer int
	if Config.MinTLSVersion == "tls10" {
		minVer = tls.VersionTLS10
	} else if Config.MinTLSVersion == "tls11" {
		minVer = tls.VersionTLS11
	} else if Config.MinTLSVersion == "tls12" {
		minVer = tls.VersionTLS12
	} else if Config.MinTLSVersion == "tls13" {
		minVer = tls.VersionTLS13
	}
	if Config.MaxTLSVersion == "tls10" {
		maxVer = tls.VersionTLS10
	} else if Config.MaxTLSVersion == "tls11" {
		maxVer = tls.VersionTLS11
	} else if Config.MaxTLSVersion == "tls12" {
		maxVer = tls.VersionTLS12
	} else if Config.MaxTLSVersion == "tls13" {
		maxVer = tls.VersionTLS13
	}
	log.Printf("set tlsConfig with min=%d max=%d versions", minVer, maxVer)
	cert, err := tls.LoadX509KeyPair(serverCrt, serverKey)
	//     cert, err := x509proxy.LoadX509KeyPair(serverCrt, serverKey)
	if err != nil {
		log.Fatalf("server loadkeys: %s", err)

	}
	// if we do not require custom verification we'll load server crt/key and present to client
	if customVerify == false {
		//         cert, err := tls.LoadX509KeyPair(serverCrt, serverKey)
		tlsConfig = &tls.Config{
			MinVersion:   uint16(minVer),
			MaxVersion:   uint16(maxVer),
			RootCAs:      _rootCAs,
			Certificates: []tls.Certificate{cert},
		}
	} else { // otherwise we'll perform custom verification of client's certificates
		tlsConfig = &tls.Config{
			// Set InsecureSkipVerify to skip the default validation we are
			// replacing. This will not disable VerifyPeerCertificate.
			MinVersion:         uint16(minVer),
			MaxVersion:         uint16(maxVer),
			InsecureSkipVerify: Config.InsecureSkipVerify,
			ClientAuth:         tls.RequestClientCert,
			RootCAs:            _rootCAs,
			Certificates:       []tls.Certificate{cert},
		}
		tlsConfig.VerifyPeerCertificate = VerifyPeerCertificate
	}
	addr := fmt.Sprintf(":%d", Config.Port)
	server := &http.Server{
		Addr:           addr,
		TLSConfig:      tlsConfig,
		ReadTimeout:    time.Duration(Config.ReadTimeout) * time.Second,
		WriteTimeout:   time.Duration(Config.WriteTimeout) * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	log.Printf("Starting HTTPs server on %s", addr)
	return server, nil
}

// LetsEncryptServer provides HTTPs server with Let's encrypt for
// given domain names (hosts)
func LetsEncryptServer(hosts ...string) *http.Server {
	// setup LetsEncrypt cert manager
	certManager := autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(hosts...),
		Cache:      autocert.DirCache("certs"),
	}

	tlsConfig := &tls.Config{
		// Set InsecureSkipVerify to skip the default validation we are
		// replacing. This will not disable VerifyPeerCertificate.
		InsecureSkipVerify: true,
		ClientAuth:         tls.RequestClientCert,
		RootCAs:            _rootCAs,
		GetCertificate:     certManager.GetCertificate,
	}
	tlsConfig.VerifyPeerCertificate = VerifyPeerCertificate

	// start HTTP server with our rootCAs and LetsEncrypt certificates
	server := &http.Server{
		Addr:      ":https",
		TLSConfig: tlsConfig,
		//         TLSConfig: &tls.Config{
		//             GetCertificate:     certManager.GetCertificate,
		//         },
	}
	// start cert Manager goroutine
	go http.ListenAndServe(":http", certManager.HTTPHandler(nil))
	log.Println("Starting LetsEncrypt HTTPs server")
	return server
}

// Stack retuns string representation of the stack function calls
func Stack() string {
	trace := make([]byte, 2048)
	count := runtime.Stack(trace, false)
	return fmt.Sprintf("\nStack of %d bytes: %s\n", count, trace)
}

// helper function to extract CN from given subject
func findCN(subject string) (string, error) {
	parts := strings.Split(subject, " ")
	for i, s := range parts {
		if strings.HasPrefix(s, "CN=") && len(parts) > i {
			cn := s
			for _, ss := range parts[i+1:] {
				if strings.Contains(ss, "=") {
					break
				}
				cn = fmt.Sprintf("%s %s", cn, ss)
			}
			return cn, nil
		}
	}
	return "", errors.New("no user CN is found in subject: " + subject)
}

// helper function to get user data from TLS request
func getUserData(r *http.Request) map[string]interface{} {
	userData := make(map[string]interface{})
	if r.TLS == nil {
		if Config.Verbose > 0 {
			log.Printf("HTTP request does not support TLS, %+v", r)
		}
		return userData
	}
	certs := r.TLS.PeerCertificates
	if Config.Verbose > 0 {
		log.Printf("found %d peer certificates in HTTP request", len(certs))
		log.Printf("HTTP request %+v", r)
		log.Printf("HTTP request TLS %+v", r.TLS)
	}
	for _, asn1Data := range certs {
		cert, err := x509.ParseCertificate(asn1Data.Raw)
		if err != nil {
			log.Println("x509RequestHandler tls: failed to parse certificate from server: " + err.Error())
		}
		if len(cert.UnhandledCriticalExtensions) > 0 {
			if Config.Verbose > 0 {
				log.Println("cert.UnhandledCriticalExtensions equal to", len(cert.UnhandledCriticalExtensions))
			}
			continue
		}
		start := time.Now()
		var subjects []string
		for _, s := range strings.Split(cert.Subject.String(), ",") {
			if strings.Contains(s, "ROOT") && strings.Contains(s, "CERN") || strings.Contains(s, "Grid") {
				continue
			}
			if Config.Verbose > 2 {
				log.Println("cert subject", s)
			}
			subjects = append(subjects, s)
		}
		if Config.Verbose > 0 {
			log.Println("cert subjects", subjects)
		}
		rec, err := cric.FindUser(subjects)
		if Config.Verbose > 0 {
			log.Printf("found user %+v error=%v elapsed time %v\n", rec, err, time.Since(start))
		}
		if err == nil {
			userData["issuer"] = strings.Split(cert.Issuer.String(), ",")[0]
			userData["Subject"] = strings.Split(cert.Subject.String(), ",")[0]
			userData["name"] = rec.Name
			userData["cern_upn"] = rec.Login
			userData["cern_person_id"] = rec.ID
			userData["auth_time"] = time.Now().Unix()
			userData["exp"] = cert.NotAfter.Unix()
			userData["email"] = cert.EmailAddresses
			userData["roles"] = rec.Roles
			userData["dn"] = rec.DN
			break
		} else {
			log.Println(err)
			continue
		}
	}
	return userData
}

// InList helper function to check item in a list
func InList(a string, list []string) bool {
	check := 0
	for _, b := range list {
		if b == a {
			check++
		}
	}
	if check != 0 {
		return true
	}
	return false
}

// PatchMatched check if given paths are matched
func PathMatched(rurl, path string, strict bool) bool {
	log.Printf("PatchMatched rurl=%s path=%s strict=%v", rurl, path, strict)
	if v, err := url.QueryUnescape(rurl); err == nil {
		rurl = v
	}
	log.Printf("PatchMatched rurl=%s path=%s strict=%v", rurl, path, strict)
	matched := false
	if strings.HasSuffix(path, "/") {
		if !strings.HasSuffix(rurl, "/") {
			rurl += "/"
		}
	}
	var prefixMatch bool
	if strings.Contains(path, ".") || strings.Contains(path, "*") || strings.Contains(path, "^") || strings.Contains(path, "$") {
		prefixMatch, _ = regexp.MatchString(path, rurl)
	} else {
		prefixMatch = strings.HasPrefix(rurl, path)
	}
	if strict {
		if prefixMatch {
			rest := strings.Replace(rurl, path, "", -1)
			if len(rest) > 0 && string(rest[0]) == "/" {
				rest = strings.Replace(rest, "/", "", 1)
			}
			// the rest of the path is just parameters and not sub-path of URI
			if !strings.Contains(rest, "/") {
				matched = true
			}
		}
	} else {
		if prefixMatch {
			matched = true
		}
	}
	log.Printf("PatchMatched rurl=%s path=%s strict=%v matched %v", rurl, path, strict, matched)
	return matched
}

// helper function to parse ingress rules
func RedirectRules(ingressRules []Ingress) (map[string]Ingress, []string) {
	rmap := make(map[string]Ingress)
	var rules []string
	for _, rec := range ingressRules {
		rules = append(rules, rec.Path)
		rmap[rec.Path] = rec
	}
	// sort rules according to length of the path
	sort.Sort(sort.Reverse(sort.StringSlice(rules)))
	return rmap, rules
}

// LogName return proper log name based on Config.LogName and either
// hostname or pod name (used in k8s environment).
func LogName() string {
	hostname, err := os.Hostname()
	if err != nil {
		log.Println("unable to get hostname", err)
	}
	if os.Getenv("MY_POD_NAME") != "" {
		hostname = os.Getenv("MY_POD_NAME")
	}
	logName := Config.LogFile + "_%Y%m%d"
	if hostname != "" {
		logName = fmt.Sprintf("%s_%s", Config.LogFile, hostname) + "_%Y%m%d"
	}
	return logName
}

// SetReferrer set  HTTP Referrer/Referer HTTP headers
func SetReferrer(r *http.Request) {
	ref := r.Header.Get("X-Forwarded-Host")
	if !strings.HasPrefix(ref, "http") {
		ref = fmt.Sprintf("https://%s", ref)
	}
	r.Header.Set("Referer", ref)
	r.Header.Set("Referrer", ref)
}
