//go:build integ
// +build integ

// Copyright Istio Authors
//
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

package rbac

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/echo/check"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/tests/integration/security/util/connection"
)

// ExpectHeaderContains specifies the expected value to be found in the HTTP header. Every value must be found in order to
// to make the test pass. Every NotValue must not be found in order to make the test pass.
type ExpectHeaderContains struct {
	Key       string
	Values    []string
	NotValues []string
}

type TestCase struct {
	NamePrefix            string
	Request               connection.Checker
	ExpectAllowed         bool
	ExpectRequestHeaders  []ExpectHeaderContains
	ExpectResponseHeaders []ExpectHeaderContains
	Jwt                   string
	Headers               map[string]string
}

func filterError(req connection.Checker, expect string, c check.Checker) check.Checker {
	return check.FilterError(func(err error) error {
		return fmt.Errorf("%s to %s:%s%s: expect %s, got: %v",
			req.From.Config().Service,
			req.Options.Target.Config().Service,
			req.Options.PortName,
			req.Options.Path,
			expect,
			err)
	}, c)
}

func checkValues(i int, response echo.Response, headers http.Header, headerType string, want []ExpectHeaderContains) error {
	for _, w := range want {
		key := w.Key
		for _, value := range w.Values {
			actual := headers.Get(key)
			if !strings.Contains(actual, value) {
				return fmt.Errorf("response[%d]: HTTP code %s, expected %s `%s` to contain `%s`, value=`%s`, raw content=%s",
					i, response.Code, headerType, key, value, actual, response.RawContent)
			}
		}
		for _, value := range w.NotValues {
			actual := headers.Get(key)
			if strings.Contains(actual, value) {
				return fmt.Errorf("response[%d]: HTTP code %s, expected %s `%s` to not contain `%s`, value=`%s`, raw content=%s",
					i, response.Code, headerType, key, value, actual, response.RawContent)
			}
		}
	}
	return nil
}

// CheckRBACRequest checks if a request is successful under RBAC policies.
// Under RBAC policies, a request is consider successful if:
// * If the policy is allow:
// *** Response code is 200
// * If the policy is deny:
// *** For HTTP: response code is 403.
// *** For TCP: EOF error
func (tc TestCase) CheckRBACRequest() error {
	req := tc.Request

	headers := make(http.Header)
	if len(tc.Jwt) > 0 {
		headers.Add("Authorization", "Bearer "+tc.Jwt)
	}
	for k, v := range tc.Headers {
		headers.Add(k, v)
	}
	tc.Request.Options.Headers = headers

	resp, err := req.From.Call(tc.Request.Options)

	checkHeaders := func(rs echo.Responses, _ error) error {
		for i, r := range rs {
			if err := checkValues(i, r, r.RequestHeaders, "request header", tc.ExpectRequestHeaders); err != nil {
				return err
			}
			if err := checkValues(i, r, r.ResponseHeaders, "response header", tc.ExpectResponseHeaders); err != nil {
				return err
			}
		}
		return nil
	}

	if tc.ExpectAllowed {
		return filterError(req, "allow with code 200",
			check.And(
				check.NoError(),
				check.OK(),
				checkHeaders,
				func(rs echo.Responses, _ error) error {
					if req.DestClusters.IsMulticluster() {
						return check.ReachedClusters(req.DestClusters).Check(rs, err)
					}
					return nil
				})).Check(resp, err)
	}

	if strings.HasPrefix(req.Options.PortName, "tcp") || req.Options.PortName == "grpc" {
		expectedErrMsg := "EOF" // TCP deny message.
		if req.Options.PortName == "grpc" {
			expectedErrMsg = "rpc error: code = PermissionDenied desc = RBAC: access denied"
		}

		return filterError(req, fmt.Sprintf("deny with %s error", expectedErrMsg),
			check.ErrorContains(expectedErrMsg)).Check(resp, err)
	}

	return filterError(req, "deny with code 403",
		check.And(
			check.NoError(),
			check.StatusCode(http.StatusForbidden),
			checkHeaders)).Check(resp, err)
}

func RunRBACTest(ctx framework.TestContext, cases []TestCase) {
	for _, tc := range cases {
		want := "deny"
		if tc.ExpectAllowed {
			want = "allow"
		}
		testName := fmt.Sprintf("%s%s->%s:%s%s[%s]",
			tc.NamePrefix,
			tc.Request.From.Config().Service,
			tc.Request.Options.Target.Config().Service,
			tc.Request.Options.PortName,
			tc.Request.Options.Path,
			want)
		ctx.NewSubTest(testName).Run(func(t framework.TestContext) {
			// Current source ip based authz test cases are not required in multicluster setup
			// because cross-network traffic will lose the origin source ip info
			if strings.Contains(testName, "source-ip") && t.Clusters().IsMulticluster() {
				t.Skip()
			}
			retry.UntilSuccessOrFail(t, tc.CheckRBACRequest,
				retry.Delay(250*time.Millisecond), retry.Timeout(30*time.Second))
		})
	}
}
