/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

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
package e2e

import (
	"os"
	"os/exec"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CSI Driver", func() {
	It("runs external e2e test binary", func(ctx SpecContext) {
		cmd := exec.CommandContext(
			ctx,
			"./e2e.test",
			"-ginkgo.v",
			"-ginkgo.timeout=3h",
			"-ginkgo.json-report=reportE2E.json",
			"-ginkgo.focus=External.Storage",
			"-storage.testdriver=test-driver.yaml",
			"-kubeconfig=/tmp/wkld-csi-e2e-cluster-kubeconfig.yaml",
		)

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Run()
		Expect(err).ToNot(HaveOccurred())
	})
})
