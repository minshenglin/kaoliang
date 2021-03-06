/*
 * Minio Cloud Storage, (C) 2015, 2016, 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package cmd This file implements helper functions to validate AWS
// Signature Version '4' authorization header.
//
// This package provides comprehensive helpers for following signature
// types.
// - Based on Authorization header.
// - Based on Query parameters.
// - Based on Form POST policy.
package cmd

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ceph/go-ceph/rados"
	sh "github.com/codeskyblue/go-sh"
	"github.com/minio/minio-go/pkg/s3utils"
	"github.com/minio/minio/pkg/auth"
	sha256 "github.com/minio/sha256-simd"
	"github.com/inwinstack/kaoliang/pkg/utils"
)

// AWS Signature Version '4' constants.
const (
	signV4Algorithm = "AWS4-HMAC-SHA256"
	iso8601Format   = "20060102T150405Z"
	yyyymmdd        = "20060102"
)

// getCanonicalHeaders generate a list of request headers with their values
func getCanonicalHeaders(signedHeaders http.Header) string {
	var headers []string
	vals := make(http.Header)
	for k, vv := range signedHeaders {
		headers = append(headers, strings.ToLower(k))
		vals[strings.ToLower(k)] = vv
	}
	sort.Strings(headers)

	var buf bytes.Buffer
	for _, k := range headers {
		buf.WriteString(k)
		buf.WriteByte(':')
		for idx, v := range vals[k] {
			if idx > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(signV4TrimAll(v))
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

// getSignedHeaders generate a string i.e alphabetically sorted, semicolon-separated list of lowercase request header names
func getSignedHeaders(signedHeaders http.Header) string {
	var headers []string
	for k := range signedHeaders {
		headers = append(headers, strings.ToLower(k))
	}
	sort.Strings(headers)
	return strings.Join(headers, ";")
}

// getCanonicalRequest generate a canonical request of style
//
// canonicalRequest =
//  <HTTPMethod>\n
//  <CanonicalURI>\n
//  <CanonicalQueryString>\n
//  <CanonicalHeaders>\n
//  <SignedHeaders>\n
//  <HashedPayload>
//
func getCanonicalRequest(extractedSignedHeaders http.Header, payload, queryStr, urlPath, method string) string {
	rawQuery := strings.Replace(queryStr, "+", "%20", -1)
	encodedPath := s3utils.EncodePath(urlPath)
	canonicalRequest := strings.Join([]string{
		method,
		encodedPath,
		rawQuery,
		getCanonicalHeaders(extractedSignedHeaders),
		getSignedHeaders(extractedSignedHeaders),
		payload,
	}, "\n")
	return canonicalRequest
}

// getScope generate a string of a specific date, an AWS region, and a service.
func getScope(t time.Time, region string) string {
	scope := strings.Join([]string{
		t.Format(yyyymmdd),
		region,
		utils.GetEnv("SERVICE", "s3"),
		"aws4_request",
	}, "/")
	return scope
}

// getStringToSign a string based on selected query values.
func getStringToSign(canonicalRequest string, t time.Time, scope string) string {
	stringToSign := signV4Algorithm + "\n" + t.Format(iso8601Format) + "\n"
	stringToSign = stringToSign + scope + "\n"
	canonicalRequestBytes := sha256.Sum256([]byte(canonicalRequest))
	stringToSign = stringToSign + hex.EncodeToString(canonicalRequestBytes[:])
	return stringToSign
}

// getSigningKey hmac seed to calculate final signature.
func getSigningKey(secretKey string, t time.Time, region string) []byte {
	date := sumHMAC([]byte("AWS4"+secretKey), []byte(t.Format(yyyymmdd)))
	regionBytes := sumHMAC(date, []byte(region))
	service := sumHMAC(regionBytes, []byte(utils.GetEnv("SERVICE", "s3")))
	signingKey := sumHMAC(service, []byte("aws4_request"))
	return signingKey
}

// getSignature final signature in hexadecimal form.
func getSignature(signingKey []byte, stringToSign string) string {
	return hex.EncodeToString(sumHMAC(signingKey, []byte(stringToSign)))
}

// Check to see if Policy is signed correctly.
func doesPolicySignatureMatch(formValues http.Header) APIErrorCode {
	// For SignV2 - Signature field will be valid
	if _, ok := formValues["Signature"]; ok {
		return doesPolicySignatureV2Match(formValues)
	}
	return doesPolicySignatureV4Match(formValues)
}

// compareSignatureV4 returns true if and only if both signatures
// are equal. The signatures are expected to be HEX encoded strings
// according to the AWS S3 signature V4 spec.
func compareSignatureV4(sig1, sig2 string) bool {
	// The CTC using []byte(str) works because the hex encoding
	// is unique for a sequence of bytes. See also compareSignatureV2.
	return subtle.ConstantTimeCompare([]byte(sig1), []byte(sig2)) == 1
}

// doesPolicySignatureMatch - Verify query headers with post policy
//     - http://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-HTTPPOSTConstructPolicy.html
// returns ErrNone if the signature matches.
func doesPolicySignatureV4Match(formValues http.Header) APIErrorCode {
	// Access credentials.
	cred := globalServerConfig.GetCredential()

	// Server region.
	region := globalServerConfig.GetRegion()

	// Parse credential tag.
	credHeader, err := parseCredentialHeader("Credential="+formValues.Get("X-Amz-Credential"), region)
	if err != ErrNone {
		return ErrMissingFields
	}

	// Verify if the access key id matches.
	if credHeader.accessKey != cred.AccessKey {
		return ErrInvalidAccessKeyID
	}

	// Get signing key.
	signingKey := getSigningKey(cred.SecretKey, credHeader.scope.date, credHeader.scope.region)

	// Get signature.
	newSignature := getSignature(signingKey, formValues.Get("Policy"))

	// Verify signature.
	if !compareSignatureV4(newSignature, formValues.Get("X-Amz-Signature")) {
		return ErrSignatureDoesNotMatch
	}

	// Success.
	return ErrNone
}

// doesPresignedSignatureMatch - Verify query headers with presigned signature
//     - http://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
// returns ErrNone if the signature matches.
func doesPresignedSignatureMatch(hashedPayload string, r *http.Request, region string) (userID string, s3Error APIErrorCode) {

	// Copy request
	req := *r

	// Parse request query string.
	pSignValues, err := parsePreSignV4(req.URL.Query(), region)
	if err != ErrNone {
		s3Error = err
		return
	}

	userID, cred, err := getCredentials(pSignValues.Credential.accessKey)
	if err != ErrNone {
		s3Error = ErrInvalidAccessKeyID
		return
	}

	// Extract all the signed headers along with its values.
	extractedSignedHeaders, errCode := extractSignedHeaders(pSignValues.SignedHeaders, r)
	if errCode != ErrNone {
		s3Error = errCode
		return
	}
	// Construct new query.
	query := make(url.Values)
	if req.URL.Query().Get("X-Amz-Content-Sha256") != "" {
		query.Set("X-Amz-Content-Sha256", hashedPayload)
	}

	query.Set("X-Amz-Algorithm", signV4Algorithm)

	// If the host which signed the request is slightly ahead in time (by less than globalMaxSkewTime) the
	// request should still be allowed.
	if pSignValues.Date.After(UTCNow().Add(globalMaxSkewTime)) {
		s3Error = ErrRequestNotReadyYet
		return
	}

	if UTCNow().Sub(pSignValues.Date) > pSignValues.Expires {
		s3Error = ErrExpiredPresignRequest
		return
	}

	// Save the date and expires.
	t := pSignValues.Date
	expireSeconds := int(pSignValues.Expires / time.Second)

	// Construct the query.
	query.Set("X-Amz-Date", t.Format(iso8601Format))
	query.Set("X-Amz-Expires", strconv.Itoa(expireSeconds))
	query.Set("X-Amz-SignedHeaders", getSignedHeaders(extractedSignedHeaders))
	query.Set("X-Amz-Credential", cred.AccessKey+"/"+getScope(t, pSignValues.Credential.scope.region))

	// Save other headers available in the request parameters.
	for k, v := range req.URL.Query() {
		if strings.HasPrefix(strings.ToLower(k), "x-amz") {
			continue
		}
		query[k] = v
	}

	// Get the encoded query.
	encodedQuery := query.Encode()

	// Verify if date query is same.
	if req.URL.Query().Get("X-Amz-Date") != query.Get("X-Amz-Date") {
		s3Error = ErrSignatureDoesNotMatch
		return
	}
	// Verify if expires query is same.
	if req.URL.Query().Get("X-Amz-Expires") != query.Get("X-Amz-Expires") {
		s3Error = ErrSignatureDoesNotMatch
		return
	}
	// Verify if signed headers query is same.
	if req.URL.Query().Get("X-Amz-SignedHeaders") != query.Get("X-Amz-SignedHeaders") {
		s3Error = ErrSignatureDoesNotMatch
		return
	}
	// Verify if credential query is same.
	if req.URL.Query().Get("X-Amz-Credential") != query.Get("X-Amz-Credential") {
		s3Error = ErrSignatureDoesNotMatch
		return
	}
	// Verify if sha256 payload query is same.
	if req.URL.Query().Get("X-Amz-Content-Sha256") != "" {
		if req.URL.Query().Get("X-Amz-Content-Sha256") != query.Get("X-Amz-Content-Sha256") {
			s3Error = ErrContentSHA256Mismatch
			return
		}
	}

	/// Verify finally if signature is same.

	// Get canonical request.
	presignedCanonicalReq := getCanonicalRequest(extractedSignedHeaders, hashedPayload, encodedQuery, req.URL.Path, req.Method)

	// Get string to sign from canonical request.
	presignedStringToSign := getStringToSign(presignedCanonicalReq, t, pSignValues.Credential.getScope())

	// Get hmac presigned signing key.
	presignedSigningKey := getSigningKey(cred.SecretKey, pSignValues.Credential.scope.date, pSignValues.Credential.scope.region)

	// Get new signature.
	newSignature := getSignature(presignedSigningKey, presignedStringToSign)

	// Verify signature.
	if !compareSignatureV4(req.URL.Query().Get("X-Amz-Signature"), newSignature) {
		s3Error = ErrSignatureDoesNotMatch
		return
	}

	s3Error = ErrNone
	return
}

// doesSignatureMatch - Verify authorization header with calculated header in accordance with
//     - http://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
// returns ErrNone if signature matches.
func doesSignatureMatch(hashedPayload string, r *http.Request, region string) (userID string, s3Error APIErrorCode) {
	// Copy request.
	req := *r

	// Save authorization header.
	v4Auth := req.Header.Get("Authorization")

	// Parse signature version '4' header.
	signV4Values, err := parseSignV4(v4Auth, region)
	if err != ErrNone {
		s3Error = err
		return
	}

	// Extract all the signed headers along with its values.
	extractedSignedHeaders, errCode := extractSignedHeaders(signV4Values.SignedHeaders, r)
	if errCode != ErrNone {
		s3Error = errCode
		return
	}

	// Verify if the access key id matches.
	userID, cred, err := getCredentials(signV4Values.Credential.accessKey)
	if err != ErrNone {
		s3Error = ErrInvalidAccessKeyID
		return
	}

	// Extract date, if not present throw error.
	var date string
	if date = req.Header.Get(http.CanonicalHeaderKey("x-amz-date")); date == "" {
		if date = r.Header.Get("Date"); date == "" {
			s3Error = ErrMissingDateHeader
			return
		}
	}
	// Parse date header.
	t, e := time.Parse(iso8601Format, date)
	if e != nil {
		s3Error = ErrMalformedDate
		return
	}

	// Query string.
	queryStr := req.URL.Query().Encode()

	// Get canonical request.
	canonicalRequest := getCanonicalRequest(extractedSignedHeaders, hashedPayload, queryStr, req.URL.Path, req.Method)

	// Get string to sign from canonical request.
	stringToSign := getStringToSign(canonicalRequest, t, signV4Values.Credential.getScope())

	// Get hmac signing key.
	signingKey := getSigningKey(cred.SecretKey, signV4Values.Credential.scope.date, signV4Values.Credential.scope.region)

	// Calculate signature.
	newSignature := getSignature(signingKey, stringToSign)

	// Verify if signature match.
	if !compareSignatureV4(newSignature, signV4Values.Signature) {
		s3Error = ErrSignatureDoesNotMatch
		return
	}

	// Return error none.
	s3Error = ErrNone
	return
}

func getCredentials(accessKey string) (string, auth.Credentials, APIErrorCode) {
	type Key struct {
		SecretKey string `json:"secret_key"`
	}

	type UserInfo struct {
		Keys []Key `json:"keys"`
	}

	conn, _ := rados.NewConn()
	conn.ReadDefaultConfigFile()
	conn.Connect()
	defer conn.Shutdown()

	ioctx, _ := conn.OpenIOContext(utils.GetEnv("RGW_METADATA_POOL", "default.rgw.meta"))
	ioctx.SetNamespace("users.keys")
	stat, _ := ioctx.Stat(accessKey)
	data := make([]byte, stat.Size-4)
	ioctx.Read(accessKey, data, 4)
	userID := string(data)

	var userInfo UserInfo
	output, _ := sh.Command("radosgw-admin", "user", "info", "--uid="+userID).Output()
	_ = json.Unmarshal(output, &userInfo)

	return userID, auth.Credentials{
		AccessKey: accessKey,
		SecretKey: userInfo.Keys[0].SecretKey,
	}, ErrNone
}
