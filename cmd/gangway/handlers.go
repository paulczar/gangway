// Copyright © 2017 Heptio
// Copyright © 2017 Craig Tracey
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

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/dgrijalva/jwt-go"
	"github.com/heptiolabs/gangway/internal/oidc"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

const (
	templatesBase  = "/templates"
	kubeConfigFile = "gangway.kubeconfig"
)

// userInfo stores information about an authenticated user
type userInfo struct {
	ClusterName  string
	Username     string
	Email        string
	IDToken      string
	RefreshToken string
	ClientID     string
	ClientSecret string
	IssuerURL    string
	APIServerURL string
	ClusterCA    string
	TrustedCA    string
	HTTPPath     string
}

// homeInfo is used to store dynamic properties on
type homeInfo struct {
	HTTPPath string
}

func serveTemplate(tmplFile string, data interface{}, w http.ResponseWriter) {
	templatePath := filepath.Join(templatesBase, tmplFile)
	templateData, err := FSString(false, templatePath)
	if err != nil {
		log.Errorf("Failed to find template asset: %s at path: %s", tmplFile, templatePath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tmpl := template.New(tmplFile).Funcs(FuncMap())
	tmpl, err = tmpl.Parse(string(templateData))
	if err != nil {
		log.Errorf("Error parsing kubeconfig template: %s", err)
	}
	err = tmpl.ExecuteTemplate(w, tmplFile, data)
	if err != nil {
		log.Errorf("Error executing kubeconf template: %s", err)
	}
}

func generateKubeConfig(tmplFile string, data interface{}) {
	templatePath := filepath.Join(templatesBase, tmplFile)
	templateData, err := FSString(false, templatePath)
	if err != nil {
		log.Errorf("Failed to find template asset: %s", tmplFile)
		return
	}

	// open file for writing
	f, err := os.Create(kubeConfigFile)
	if err != nil {
		log.Errorf("Error creating kubeconfig file: %s", err)
	}

	// create buffered io writer
	w := bufio.NewWriter(f)

	tmpl := template.New(tmplFile).Funcs(FuncMap())
	tmpl, err = tmpl.Parse(string(templateData))
	if err != nil {
		log.Errorf("Error parsing kubeconfig template: %s", err)
	}
	err = tmpl.ExecuteTemplate(w, tmplFile, data)
	if err != nil {
		log.Errorf("Error executing kubeconf template: %s", err)
	}
	// flush file data to disk
	w.Flush()
}

func loginRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := gangwayUserSession.Session.Get(r, "gangway_id_token")
		if err != nil {
			http.Redirect(w, r, cfg.GetRootPathPrefix(), http.StatusTemporaryRedirect)
			return
		}

		if session.Values["id_token"] == nil {
			http.Redirect(w, r, cfg.GetRootPathPrefix(), http.StatusTemporaryRedirect)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	data := &homeInfo{
		HTTPPath: cfg.HTTPPath,
	}

	serveTemplate("home.tmpl", data, w)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {

	b := make([]byte, 32)
	rand.Read(b)
	state := base64.StdEncoding.EncodeToString(b)

	session, err := gangwayUserSession.Session.Get(r, "gangway")
	if err != nil {
		log.Errorf("Got an error in login: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	session.Values["state"] = state
	err = session.Save(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	audience := oauth2.SetAuthURLParam("audience", cfg.Audience)
	url := oauth2Cfg.AuthCodeURL(state, audience)

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	gangwayUserSession.Cleanup(w, r, "gangway")
	gangwayUserSession.Cleanup(w, r, "gangway_id_token")
	gangwayUserSession.Cleanup(w, r, "gangway_refresh_token")
	http.Redirect(w, r, cfg.GetRootPathPrefix(), http.StatusTemporaryRedirect)
}

func callbackHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, transportConfig.HTTPClient)

	// load up session cookies
	session, err := gangwayUserSession.Session.Get(r, "gangway")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sessionIDToken, err := gangwayUserSession.Session.Get(r, "gangway_id_token")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sessionRefreshToken, err := gangwayUserSession.Session.Get(r, "gangway_refresh_token")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// verify the state string
	state := r.URL.Query().Get("state")

	if state != session.Values["state"] {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	// use the access code to retrieve a token
	code := r.URL.Query().Get("code")
	token, err := o2token.Exchange(ctx, code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sessionIDToken.Values["id_token"] = token.Extra("id_token")
	sessionRefreshToken.Values["refresh_token"] = token.RefreshToken

	// save the session cookies
	err = session.Save(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = sessionIDToken.Save(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = sessionRefreshToken.Save(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("%s/commandline", cfg.HTTPPath), http.StatusSeeOther)
}

func commandlineHandler(w http.ResponseWriter, r *http.Request) {

	// read in public ca.crt to output in commandline copy/paste commands
	file, err := os.Open(cfg.ClusterCAPath)
	if err != nil {
		// let us know that we couldn't open the file. This only causes missing output
		// does not impact actual function of program
		log.Errorf("Failed to open CA file. %s", err)
	}
	defer file.Close()
	caBytes, err := ioutil.ReadAll(file)
	if err != nil {
		// let us know that we couldn't open the file. This only causes missing output
		// does not impact actual function of program
		log.Warningf("Could not read CA file: %s", err)
	}
	// read in trusted ca cert to output in commandline copy/paste commands
	file, err = os.Open(cfg.TrustedCAPath)
	if err != nil {
		// let us know that we couldn't open the file. This only causes missing output
		// does not impact actual function of program
		log.Errorf("Failed to open TrustedCA file. %s", err)
	}
	defer file.Close()
	trustedCABytes, err := ioutil.ReadAll(file)
	if err != nil {
		// let us know that we couldn't open the file. This only causes missing output
		// does not impact actual function of program
		log.Warningf("Failed to read TrustedCA file. %s", err)
	}

	// load the session cookies
	sessionIDToken, err := gangwayUserSession.Session.Get(r, "gangway_id_token")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	sessionRefreshToken, err := gangwayUserSession.Session.Get(r, "gangway_refresh_token")
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	idToken, ok := sessionIDToken.Values["id_token"].(string)
	if !ok {
		gangwayUserSession.Cleanup(w, r, "gangway")
		gangwayUserSession.Cleanup(w, r, "gangway_id_token")
		gangwayUserSession.Cleanup(w, r, "gangway_refresh_token")

		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	refreshToken, ok := sessionRefreshToken.Values["refresh_token"].(string)
	if !ok {
		gangwayUserSession.Cleanup(w, r, "gangway")
		gangwayUserSession.Cleanup(w, r, "gangway_id_token")
		gangwayUserSession.Cleanup(w, r, "gangway_refresh_token")

		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	jwtToken, err := oidc.ParseToken(idToken, cfg.ClientSecret)
	if err != nil {
		http.Error(w, "Could not parse JWT", http.StatusInternalServerError)
		return
	}

	claims := jwtToken.Claims.(jwt.MapClaims)
	username, ok := claims[cfg.UsernameClaim].(string)
	if !ok {
		http.Error(w, "Could not parse Username claim", http.StatusInternalServerError)
		return
	}
	email := strings.Join([]string{username, cfg.ClusterName}, "@")
	if cfg.EmailClaim != "" {
		email, ok = claims[cfg.EmailClaim].(string)
		if !ok {
			http.Error(w, "Could not parse Email claim", http.StatusInternalServerError)
			log.Warn("using the Email Claim config setting is deprecated. In future Gangway will use `UsernameClaim@ClusterName`")
			return
		}
	}

	issuerURL, ok := claims["iss"].(string)
	if !ok {
		http.Error(w, "Could not parse Issuer URL claim", http.StatusInternalServerError)
		return
	}

	if cfg.ClientSecret == "" {
		log.Warn("Setting an empty Client Secret should only be done if you have no other option and is an inherent security risk.")
	}

	info := &userInfo{
		ClusterName:  cfg.ClusterName,
		Username:     username,
		Email:        email,
		IDToken:      idToken,
		RefreshToken: refreshToken,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		IssuerURL:    issuerURL,
		APIServerURL: cfg.APIServerURL,
		ClusterCA:    string(caBytes),
		TrustedCA:    string(trustedCABytes),
		HTTPPath:     cfg.HTTPPath,
	}

	generateKubeConfig("kubeconfig.tmpl", info)
	serveTemplate("commandline.tmpl", info, w)
}

func kubeConfigHandler(w http.ResponseWriter, r *http.Request) {
	// tell the browser the returned content should be downloaded
	w.Header().Add("Content-Disposition", "Attachment")
	http.ServeFile(w, r, kubeConfigFile)
}
