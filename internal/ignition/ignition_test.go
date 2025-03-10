package ignition

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	config_31 "github.com/coreos/ignition/v2/config/v3_1"
	config_32 "github.com/coreos/ignition/v2/config/v3_2"
	config_32_types "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	bmh_v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/host/hostutil"
	"github.com/openshift/assisted-service/internal/operators"
	"github.com/openshift/assisted-service/internal/provider/registry"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/pkg/auth"
	"github.com/openshift/assisted-service/pkg/mirrorregistries"
	"github.com/openshift/assisted-service/pkg/s3wrapper"
	"github.com/openshift/assisted-service/pkg/staticnetworkconfig"
	"github.com/sirupsen/logrus"
)

var (
	cluster              *common.Cluster
	hostInventory        string
	installerCacheDir    string
	log                  = logrus.New()
	workDir              string
	mockOperatorManager  operators.API
	mockProviderRegistry *registry.MockProviderRegistry
	ctrl                 *gomock.Controller
)

var _ = BeforeEach(func() {
	// setup temp workdir
	var err error
	workDir, err = ioutil.TempDir("", "assisted-install-test-")
	Expect(err).NotTo(HaveOccurred())
	installerCacheDir = filepath.Join(workDir, "installercache")

	// create simple cluster
	clusterID := strfmt.UUID(uuid.New().String())
	cluster = &common.Cluster{
		Cluster: models.Cluster{
			ID: &clusterID,
		},
	}
	cluster.ImageInfo = &models.ImageInfo{}

	hostInventory = `{"bmc_address":"0.0.0.0","bmc_v6address":"::/0","boot":{"current_boot_mode":"bios"},"cpu":{"architecture":"x86_64","count":4,"flags":["fpu","vme","de","pse","tsc","msr","pae","mce","cx8","apic","sep","mtrr","pge","mca","cmov","pat","pse36","clflush","mmx","fxsr","sse","sse2","ss","syscall","nx","pdpe1gb","rdtscp","lm","constant_tsc","arch_perfmon","rep_good","nopl","xtopology","cpuid","tsc_known_freq","pni","pclmulqdq","vmx","ssse3","fma","cx16","pcid","sse4_1","sse4_2","x2apic","movbe","popcnt","tsc_deadline_timer","aes","xsave","avx","f16c","rdrand","hypervisor","lahf_lm","abm","3dnowprefetch","cpuid_fault","invpcid_single","pti","ssbd","ibrs","ibpb","stibp","tpr_shadow","vnmi","flexpriority","ept","vpid","ept_ad","fsgsbase","tsc_adjust","bmi1","hle","avx2","smep","bmi2","erms","invpcid","rtm","mpx","avx512f","avx512dq","rdseed","adx","smap","clflushopt","clwb","avx512cd","avx512bw","avx512vl","xsaveopt","xsavec","xgetbv1","xsaves","arat","umip","pku","ospke","md_clear","arch_capabilities"],"frequency":2095.076,"model_name":"Intel(R) Xeon(R) Gold 6152 CPU @ 2.10GHz"},"disks":[{"by_path":"/dev/disk/by-path/pci-0000:00:06.0","drive_type":"HDD","model":"unknown","name":"vda","path":"/dev/vda","serial":"unknown","size_bytes":21474836480,"vendor":"0x1af4","wwn":"unknown"}],"hostname":"test-infra-cluster-master-1.redhat.com","interfaces":[{"flags":["up","broadcast","multicast"],"has_carrier":true,"ipv4_addresses":["192.168.126.11/24"],"ipv6_addresses":["fe80::5054:ff:fe42:1e8d/64"],"mac_address":"52:54:00:42:1e:8d","mtu":1500,"name":"eth0","product":"0x0001","speed_mbps":-1,"vendor":"0x1af4"},{"flags":["up","broadcast","multicast"],"has_carrier":true,"ipv4_addresses":["192.168.140.133/24"],"ipv6_addresses":["fe80::5054:ff:feca:7b16/64"],"mac_address":"52:54:00:ca:7b:16","mtu":1500,"name":"eth1","product":"0x0001","speed_mbps":-1,"vendor":"0x1af4"}],"memory":{"physical_bytes":17809014784,"usable_bytes":17378611200},"system_vendor":{"manufacturer":"Red Hat","product_name":"KVM"}}`

	ctrl = gomock.NewController(GinkgoT())
	mockOperatorManager = operators.NewMockAPI(ctrl)
	mockProviderRegistry = registry.NewMockProviderRegistry(ctrl)
})

var _ = AfterEach(func() {
	os.RemoveAll(workDir)
	ctrl.Finish()
})

var _ = Describe("Bootstrap Ignition Update", func() {
	const bootstrap1 = `{
		"ignition": {
		  "config": {},
		  "security": {
			"tls": {}
		  },
		  "timeouts": {},
		  "version": "3.2.0"
		},
		"storage": {
		  "files": [
			{
			  "filesystem": "root",
			  "path": "/opt/openshift/openshift/99_openshift-cluster-api_hosts-0.yaml",
			  "user": {
				"name": "root"
			  },
			  "contents": {
				"source": "data:text/plain;charset=utf-8;base64,YXBpVmVyc2lvbjogbWV0YWwzLmlvL3YxYWxwaGExCmtpbmQ6IEJhcmVNZXRhbEhvc3QKbWV0YWRhdGE6CiAgY3JlYXRpb25UaW1lc3RhbXA6IG51bGwKICBuYW1lOiBvcGVuc2hpZnQtbWFzdGVyLTAKICBuYW1lc3BhY2U6IG9wZW5zaGlmdC1tYWNoaW5lLWFwaQpzcGVjOgogIGJtYzoKICAgIGFkZHJlc3M6IGlwbWk6Ly8xOTIuMTY4LjExMS4xOjYyMzAKICAgIGNyZWRlbnRpYWxzTmFtZTogb3BlbnNoaWZ0LW1hc3Rlci0wLWJtYy1zZWNyZXQKICBib290TUFDQWRkcmVzczogMDA6YWE6Mzk6YjM6NTE6MTAKICBjb25zdW1lclJlZjoKICAgIGFwaVZlcnNpb246IG1hY2hpbmUub3BlbnNoaWZ0LmlvL3YxYmV0YTEKICAgIGtpbmQ6IE1hY2hpbmUKICAgIG5hbWU6IGRlbW8tbWFzdGVyLTAKICAgIG5hbWVzcGFjZTogb3BlbnNoaWZ0LW1hY2hpbmUtYXBpCiAgZXh0ZXJuYWxseVByb3Zpc2lvbmVkOiB0cnVlCiAgaGFyZHdhcmVQcm9maWxlOiB1bmtub3duCiAgb25saW5lOiB0cnVlCnN0YXR1czoKICBlcnJvck1lc3NhZ2U6ICIiCiAgZ29vZENyZWRlbnRpYWxzOiB7fQogIGhhcmR3YXJlUHJvZmlsZTogIiIKICBvcGVyYXRpb25IaXN0b3J5OgogICAgZGVwcm92aXNpb246CiAgICAgIGVuZDogbnVsbAogICAgICBzdGFydDogbnVsbAogICAgaW5zcGVjdDoKICAgICAgZW5kOiBudWxsCiAgICAgIHN0YXJ0OiBudWxsCiAgICBwcm92aXNpb246CiAgICAgIGVuZDogbnVsbAogICAgICBzdGFydDogbnVsbAogICAgcmVnaXN0ZXI6CiAgICAgIGVuZDogbnVsbAogICAgICBzdGFydDogbnVsbAogIG9wZXJhdGlvbmFsU3RhdHVzOiAiIgogIHBvd2VyZWRPbjogZmFsc2UKICBwcm92aXNpb25pbmc6CiAgICBJRDogIiIKICAgIGltYWdlOgogICAgICBjaGVja3N1bTogIiIKICAgICAgdXJsOiAiIgogICAgc3RhdGU6ICIiCiAgdHJpZWRDcmVkZW50aWFsczoge30K",
				"verification": {}
			  },
			  "mode": 420
			}
		  ]
		}
	  }`

	var (
		err          error
		examplePath  string
		bmh          *bmh_v1alpha1.BareMetalHost
		config       *config_32_types.Config
		mockS3Client *s3wrapper.MockAPI
	)

	BeforeEach(func() {
		var err1 error
		examplePath = filepath.Join(workDir, "example1.ign")
		err1 = ioutil.WriteFile(examplePath, []byte(bootstrap1), 0600)
		Expect(err1).NotTo(HaveOccurred())
		mockS3Client = s3wrapper.NewMockAPI(ctrl)

		cluster.Hosts = []*models.Host{
			{
				Inventory:         hostInventory,
				RequestedHostname: "example1",
				Role:              models.HostRoleMaster,
			},
		}
		g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", mockS3Client, log,
			mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

		err = g.updateBootstrap(context.Background(), examplePath)

		// TODO(deprecate-ignition-3.1.0)
		bootstrapBytes, _ := ioutil.ReadFile(examplePath)
		config, err1 = ParseToLatest(bootstrapBytes)
		Expect(err1).NotTo(HaveOccurred())
		Expect(config.Ignition.Version).To(Equal("3.2.0"))
		bytes, err1 := json.Marshal(config)
		Expect(err1).ToNot(HaveOccurred())
		v32Config, _, err1 := config_32.Parse(bytes)
		Expect(err1).ToNot(HaveOccurred())
		Expect(v32Config.Ignition.Version).To(Equal("3.2.0"))

		var file *config_32_types.File
		foundNMConfig := false
		for i := range config.Storage.Files {
			if isBMHFile(&config.Storage.Files[i]) {
				file = &config.Storage.Files[i]
			}
			if config.Storage.Files[i].Node.Path == "/etc/NetworkManager/conf.d/99-kni.conf" {
				foundNMConfig = true
			}
		}
		bmh, _ = fileToBMH(file)
		Expect(foundNMConfig).To(BeTrue(), "file /etc/NetworkManager/conf.d/99-kni.conf not present in bootstrap.ign")
	})

	Context("Identify host role", func() {
		var hosts []*models.Host

		BeforeEach(func() {
			hosts = []*models.Host{
				{
					RequestedHostname: "openshift-master-0",
				},
			}
		})
		test := func(masters, workers []*models.Host, masterExpected bool) {
			masterHostnames := getHostnames(masters)
			workerHostnames := getHostnames(workers)
			Expect(err).ToNot(HaveOccurred())
			for i := range config.Storage.Files {
				if isBMHFile(&config.Storage.Files[i]) {
					bmhFile, err := fileToBMH(&config.Storage.Files[i]) //nolint,shadow
					Expect(err).ToNot(HaveOccurred())
					Expect(bmhIsMaster(bmhFile, masterHostnames, workerHostnames)).To(Equal(masterExpected))
					return
				}
			}
			Fail("No BMH file found")
		}
		It("Set as master by hostname", func() {
			test(hosts, nil, true)
		})
		It("Set as worker by hostname", func() {
			test(nil, hosts, false)
		})
		It("Set as master by backward compatibility", func() {
			test(nil, nil, true)
		})
	})

	Describe("update bootstrap.ign", func() {
		Context("with 1 master", func() {
			It("got a tmp workDir", func() {
				Expect(workDir).NotTo(Equal(""))
			})
			It("adds annotation", func() {
				Expect(err).NotTo(HaveOccurred())
				Expect(bmh.Annotations).To(HaveKey(bmh_v1alpha1.StatusAnnotation))
			})
			It("adds the marker file", func() {
				var found bool
				for _, f := range config.Storage.Files {
					if f.Path == "/opt/openshift/assisted-install-bootstrap" {
						found = true
					}
				}
				Expect(found).To(BeTrue())
			})
		})
	})
})

var _ = Describe("Cluster Ignitions Update", func() {
	const ignition = `{
		"ignition": {
		  "config": {},
		  "version": "3.2.0"
		},
		"storage": {
		  "files": []
		}
	  }`

	const caCert = `
-----BEGIN CERTIFICATE-----
MIIDkDCCAnigAwIBAgIUNQRERAPbVOlJoLs2N76uLZN9S1gwDQYJKoZIhvcNAQEL
BQAwTzELMAkGA1UEBhMCQ0ExCzAJBgNVBAgMAlFDMRswGQYDVQQKDBJBc3Npc3Rl
ZCBJbnN0YWxsZXIxFjAUBgNVBAMMDTE5Mi4xNjguMTIyLjUwHhcNMjAxMDAxMTUz
NDUyWhcNMjExMDAxMTUzNDUyWjBPMQswCQYDVQQGEwJDQTELMAkGA1UECAwCUUMx
GzAZBgNVBAoMEkFzc2lzdGVkIEluc3RhbGxlcjEWMBQGA1UEAwwNMTkyLjE2OC4x
MjIuNTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALF0Oj3awX//uMSn
B7grPKSuSbLlBIIeRgHaOAvdVZFn86f2G8prG0RHA4u9anidQlhR3wCGx16bQIt0
NC3n16RSn5x9LgsV0woFrXNUs535nkE0Zg5Yex10/yF8URauzlPierq10fe1N6kB
OF1OfGBPpyUN+1zSeYcX4fyALpreLaTEhIGMnHjDqytccbupNYjrCWA5lE4uJ6a4
BBAqiWPBV5KneD5pHNb7mVbMaFGdteUwqKQtfO8uM0T9loYbXNYqVt6irOYbIowo
uHvsdGD3ryFnASGOZ4AJ0eQXSn3bFrMj5T9ojna1C82DYhK2Mbff1qrMYZG2rNE6
y6Is8gkCAwEAAaNkMGIwHQYDVR0OBBYEFK4tVRjbPL3fuId5mdKOFALaGQw6MB8G
A1UdIwQYMBaAFK4tVRjbPL3fuId5mdKOFALaGQw6MA8GA1UdEwEB/wQFMAMBAf8w
DwYDVR0RBAgwBocEwKh6BTANBgkqhkiG9w0BAQsFAAOCAQEAoeJYGcAYdrkQcOum
ph4LNyEBhnfqlcQ5gQLIGALf/tpuz66SEeR1Km9hRwsl4nqDf2IVLu9CY79VP4J3
tgu2tPcz/jpqcMdp54Pw20AfzW/zJqPV/TEYZ1CYeaRbsnTRltx8KlnF0OVDNv8M
Q6BVcoQmSTxlJeGp9hrxahCbGHjKIaLLxmEdwVt1HpEMcGXjv5E6dbil9U6Mx1Ce
nghVxZEMX1Vrnlyu1LVknfcWQT1HTK0ccMp1RRewM21C87MADYwN1ale2C6jVEyk
SV4bRR9i0uf+xQ/oYRvugQ25Q7EahO5hJIWRf4aULbk36Zpw3++v2KFnF26zqwB6
1XWdHQ==
-----END CERTIFICATE-----`

	var (
		masterPath string
		workerPath string
		caCertPath string
	)

	BeforeEach(func() {
		masterPath = filepath.Join(workDir, "master.ign")
		workerPath = filepath.Join(workDir, "worker.ign")
		err := ioutil.WriteFile(masterPath, []byte(ignition), 0600)
		Expect(err).NotTo(HaveOccurred())
		err = ioutil.WriteFile(workerPath, []byte(ignition), 0600)
		Expect(err).NotTo(HaveOccurred())

		caCertPath = filepath.Join(workDir, "service-ca-cert.crt")
		err = ioutil.WriteFile(caCertPath, []byte(caCert), 0600)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("update ignitions", func() {
		It("with ca cert file", func() {
			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", caCertPath, "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			err := g.updateIgnitions()
			Expect(err).NotTo(HaveOccurred())

			masterBytes, err := ioutil.ReadFile(masterPath)
			Expect(err).NotTo(HaveOccurred())
			masterConfig, _, err := config_32.Parse(masterBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(masterConfig.Storage.Files).To(HaveLen(1))
			file := &masterConfig.Storage.Files[0]
			Expect(file.Path).To(Equal(common.HostCACertPath))

			workerBytes, err := ioutil.ReadFile(workerPath)
			Expect(err).NotTo(HaveOccurred())
			workerConfig, _, err := config_32.Parse(workerBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(workerConfig.Storage.Files).To(HaveLen(1))
			file = &masterConfig.Storage.Files[0]
			Expect(file.Path).To(Equal(common.HostCACertPath))
		})
		It("with no ca cert file", func() {
			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			err := g.updateIgnitions()
			Expect(err).NotTo(HaveOccurred())

			masterBytes, err := ioutil.ReadFile(masterPath)
			Expect(err).NotTo(HaveOccurred())
			masterConfig, _, err := config_32.Parse(masterBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(masterConfig.Storage.Files).To(HaveLen(0))

			workerBytes, err := ioutil.ReadFile(workerPath)
			Expect(err).NotTo(HaveOccurred())
			workerConfig, _, err := config_32.Parse(workerBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(workerConfig.Storage.Files).To(HaveLen(0))
		})
		It("with service ips", func() {
			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			err := g.UpdateEtcHosts("10.10.10.1,10.10.10.2")
			Expect(err).NotTo(HaveOccurred())

			masterBytes, err := ioutil.ReadFile(masterPath)
			Expect(err).NotTo(HaveOccurred())
			masterConfig, _, err := config_32.Parse(masterBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(masterConfig.Storage.Files).To(HaveLen(1))
			file := &masterConfig.Storage.Files[0]
			Expect(file.Path).To(Equal("/etc/hosts"))

			workerBytes, err := ioutil.ReadFile(workerPath)
			Expect(err).NotTo(HaveOccurred())
			workerConfig, _, err := config_32.Parse(workerBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(workerConfig.Storage.Files).To(HaveLen(1))
			file = &masterConfig.Storage.Files[0]
			Expect(file.Path).To(Equal("/etc/hosts"))
		})
		It("with no service ips", func() {
			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			err := g.UpdateEtcHosts("")
			Expect(err).NotTo(HaveOccurred())

			masterBytes, err := ioutil.ReadFile(masterPath)
			Expect(err).NotTo(HaveOccurred())
			masterConfig, _, err := config_32.Parse(masterBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(masterConfig.Storage.Files).To(HaveLen(0))

			workerBytes, err := ioutil.ReadFile(workerPath)
			Expect(err).NotTo(HaveOccurred())
			workerConfig, _, err := config_32.Parse(workerBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(workerConfig.Storage.Files).To(HaveLen(0))
		})
		It("get service ip hostnames", func() {
			content := GetServiceIPHostnames("")
			Expect(content).To(Equal(""))

			content = GetServiceIPHostnames("10.10.10.10")
			Expect(content).To(Equal("10.10.10.10 assisted-api.local.openshift.io\n"))

			content = GetServiceIPHostnames("10.10.10.1,10.10.10.2")
			Expect(content).To(Equal("10.10.10.1 assisted-api.local.openshift.io\n10.10.10.2 assisted-api.local.openshift.io\n"))
		})
		Context("DHCP generation", func() {
			It("Definitions only", func() {
				g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
					mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

				g.encodedDhcpFileContents = "data:,abc"
				err := g.updateIgnitions()
				Expect(err).NotTo(HaveOccurred())

				masterBytes, err := ioutil.ReadFile(masterPath)
				Expect(err).ToNot(HaveOccurred())
				masterConfig, _, err := config_32.Parse(masterBytes)
				Expect(err).NotTo(HaveOccurred())
				Expect(masterConfig.Storage.Files).To(HaveLen(1))
				f := masterConfig.Storage.Files[0]
				Expect(f.Mode).To(Equal(swag.Int(0o644)))
				Expect(f.Contents.Source).To(Equal(swag.String("data:,abc")))
				Expect(f.Path).To(Equal("/etc/keepalived/unsupported-monitor.conf"))
			})
		})
		It("Definitions+leases", func() {
			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			g.encodedDhcpFileContents = "data:,abc"
			cluster.ApiVipLease = "api"
			cluster.IngressVipLease = "ingress"
			err := g.updateIgnitions()
			Expect(err).NotTo(HaveOccurred())

			masterBytes, err := ioutil.ReadFile(masterPath)
			Expect(err).ToNot(HaveOccurred())
			masterConfig, _, err := config_32.Parse(masterBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(masterConfig.Storage.Files).To(HaveLen(3))
			f := masterConfig.Storage.Files[0]
			Expect(f.Mode).To(Equal(swag.Int(0o644)))
			Expect(f.Contents.Source).To(Equal(swag.String("data:,abc")))
			Expect(f.Path).To(Equal("/etc/keepalived/unsupported-monitor.conf"))
			f = masterConfig.Storage.Files[1]
			Expect(f.Mode).To(Equal(swag.Int(0o644)))
			Expect(f.Contents.Source).To(Equal(swag.String("data:,api")))
			Expect(f.Path).To(Equal("/etc/keepalived/lease-api"))
			f = masterConfig.Storage.Files[2]
			Expect(f.Mode).To(Equal(swag.Int(0o644)))
			Expect(f.Contents.Source).To(Equal(swag.String("data:,ingress")))
			Expect(f.Path).To(Equal("/etc/keepalived/lease-ingress"))
		})
	})
})

var _ = Describe("createHostIgnitions", func() {
	const masterIgn = `{
		  "ignition": {
		    "config": {
		      "merge": [
			{
			  "source": "https://192.168.126.199:22623/config/master"
			}
		      ]
		    },
		    "security": {
		      "tls": {
			"certificateAuthorities": [
			  {
			    "source": "data:text/plain;charset=utf-8;base64,LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSURFRENDQWZpZ0F3SUJBZ0lJUk90aUgvOC82ckF3RFFZSktvWklodmNOQVFFTEJRQXdKakVTTUJBR0ExVUUKQ3hNSmIzQmxibk5vYVdaME1SQXdEZ1lEVlFRREV3ZHliMjkwTFdOaE1CNFhEVEl3TURreE9ERTVORFV3TVZvWApEVE13TURreE5qRTVORFV3TVZvd0pqRVNNQkFHQTFVRUN4TUpiM0JsYm5Ob2FXWjBNUkF3RGdZRFZRUURFd2R5CmIyOTBMV05oTUlJQklqQU5CZ2txaGtpRzl3MEJBUUVGQUFPQ0FROEFNSUlCQ2dLQ0FRRUE1c1orVWtaaGsxUWQKeFU3cWI3YXArNFczaS9ZWTFzZktURC8ybDVJTjFJeVhPajlSL1N2VG5SOGYvajNJa1JHMWN5ZXR4bnNlNm1aZwpaOW1IRDJMV0srSEFlTTJSYXpuRkEwVmFwOWxVbVRrd3Vza2Z3QzhnMWJUZUVHUlEyQmFId09KekpvdjF4a0ZICmU2TUZCMlcxek1rTWxLTkwycnlzMzRTeVYwczJpNTFmTTJvTEM2SXRvWU91RVVVa2o0dnVUbThPYm5rV0t4ZnAKR1VGMThmNzVYeHJId0tVUEd0U0lYMGxpVGJNM0tiTDY2V2lzWkFIeStoN1g1dnVaaFYzYXhwTVFMdlczQ2xvcQpTaG9zSXY4SWNZbUJxc210d2t1QkN3cWxibEo2T2gzblFrelorVHhQdGhkdWsrZytzaVBUNi9va0JKU2M2cURjClBaNUNyN3FrR3dJREFRQUJvMEl3UURBT0JnTlZIUThCQWY4RUJBTUNBcVF3RHdZRFZSMFRBUUgvQkFVd0F3RUIKL3pBZEJnTlZIUTRFRmdRVWNSbHFHT1g3MWZUUnNmQ0tXSGFuV3NwMFdmRXdEUVlKS29aSWh2Y05BUUVMQlFBRApnZ0VCQU5Xc0pZMDY2RnNYdzFOdXluMEkwNUtuVVdOMFY4NVJVV2drQk9Wd0J5bHluTVRneGYyM3RaY1FsS0U4CjVHMlp4Vzl5NmpBNkwzMHdSNWhOcnBzM2ZFcUhobjg3UEM3L2tWQWlBOWx6NjBwV2ovTE5GU1hobDkyejBGMEIKcGNUQllFc1JNYU0zTFZOK0tZb3Q2cnJiamlXdmxFMU9hS0Q4dnNBdkk5YXVJREtOdTM0R2pTaUJGWXMrelRjSwphUUlTK3UzRHVYMGpVY001aUgrMmwzNGxNR0hlY2tjS1hnUWNXMGJiT28xNXY1Q2ExenJtQ2hIUHUwQ2NhMU1MCjJaM2MxMHVXZnR2OVZnbC9LcEpzSjM3b0phbTN1Mmp6MXN0K3hHby9iTmVSdHpOMjdXQSttaDZ6bXFwRldYKzUKdWFjZUY1SFRWc0FkbmtJWHpwWXBuek5qb0lFPQotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0tCg=="
			  }
			]
		      }
		    },
		    "version": "3.2.0"
		  },
		  "storage": {
		    "files": [
		      {
			"filesystem": "root",
			"path": "/etc/keepalived/unsupported-monitor.conf",
			"mode": 644,
			"contents": {
			  "source": "data:,api-vip:%0A%20%20name:%20api%0A%20%20mac-address:%2000:1a:4a:b8:a9:d6%0A%20%20ip-address:%20192.168.126.199%0Aingress-vip:%0A%20%20name:%20ingress%0A%20%20mac-address:%2000:1a:4a:09:b7:50%0A%20%20ip-address:%20192.168.126.126%0A"
			}
		      }
		    ]
		  }
		}`
	const workerIgn = `{
		  "ignition": {
		    "config": {
		      "merge": [
			{
			  "source": "https://192.168.126.199:22623/config/worker"
			}
		      ]
		    },
		    "security": {
		      "tls": {
			"certificateAuthorities": [
			  {
			    "source": "data:text/plain;charset=utf-8;base64,LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSURFRENDQWZpZ0F3SUJBZ0lJUk90aUgvOC82ckF3RFFZSktvWklodmNOQVFFTEJRQXdKakVTTUJBR0ExVUUKQ3hNSmIzQmxibk5vYVdaME1SQXdEZ1lEVlFRREV3ZHliMjkwTFdOaE1CNFhEVEl3TURreE9ERTVORFV3TVZvWApEVE13TURreE5qRTVORFV3TVZvd0pqRVNNQkFHQTFVRUN4TUpiM0JsYm5Ob2FXWjBNUkF3RGdZRFZRUURFd2R5CmIyOTBMV05oTUlJQklqQU5CZ2txaGtpRzl3MEJBUUVGQUFPQ0FROEFNSUlCQ2dLQ0FRRUE1c1orVWtaaGsxUWQKeFU3cWI3YXArNFczaS9ZWTFzZktURC8ybDVJTjFJeVhPajlSL1N2VG5SOGYvajNJa1JHMWN5ZXR4bnNlNm1aZwpaOW1IRDJMV0srSEFlTTJSYXpuRkEwVmFwOWxVbVRrd3Vza2Z3QzhnMWJUZUVHUlEyQmFId09KekpvdjF4a0ZICmU2TUZCMlcxek1rTWxLTkwycnlzMzRTeVYwczJpNTFmTTJvTEM2SXRvWU91RVVVa2o0dnVUbThPYm5rV0t4ZnAKR1VGMThmNzVYeHJId0tVUEd0U0lYMGxpVGJNM0tiTDY2V2lzWkFIeStoN1g1dnVaaFYzYXhwTVFMdlczQ2xvcQpTaG9zSXY4SWNZbUJxc210d2t1QkN3cWxibEo2T2gzblFrelorVHhQdGhkdWsrZytzaVBUNi9va0JKU2M2cURjClBaNUNyN3FrR3dJREFRQUJvMEl3UURBT0JnTlZIUThCQWY4RUJBTUNBcVF3RHdZRFZSMFRBUUgvQkFVd0F3RUIKL3pBZEJnTlZIUTRFRmdRVWNSbHFHT1g3MWZUUnNmQ0tXSGFuV3NwMFdmRXdEUVlKS29aSWh2Y05BUUVMQlFBRApnZ0VCQU5Xc0pZMDY2RnNYdzFOdXluMEkwNUtuVVdOMFY4NVJVV2drQk9Wd0J5bHluTVRneGYyM3RaY1FsS0U4CjVHMlp4Vzl5NmpBNkwzMHdSNWhOcnBzM2ZFcUhobjg3UEM3L2tWQWlBOWx6NjBwV2ovTE5GU1hobDkyejBGMEIKcGNUQllFc1JNYU0zTFZOK0tZb3Q2cnJiamlXdmxFMU9hS0Q4dnNBdkk5YXVJREtOdTM0R2pTaUJGWXMrelRjSwphUUlTK3UzRHVYMGpVY001aUgrMmwzNGxNR0hlY2tjS1hnUWNXMGJiT28xNXY1Q2ExenJtQ2hIUHUwQ2NhMU1MCjJaM2MxMHVXZnR2OVZnbC9LcEpzSjM3b0phbTN1Mmp6MXN0K3hHby9iTmVSdHpOMjdXQSttaDZ6bXFwRldYKzUKdWFjZUY1SFRWc0FkbmtJWHpwWXBuek5qb0lFPQotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0tCg=="
			  }
			]
		      }
		    },
		    "version": "3.2.0"
		  }
		}`

	BeforeEach(func() {
		masterPath := filepath.Join(workDir, "master.ign")
		err := ioutil.WriteFile(masterPath, []byte(masterIgn), 0600)
		Expect(err).NotTo(HaveOccurred())

		workerPath := filepath.Join(workDir, "worker.ign")
		err = ioutil.WriteFile(workerPath, []byte(workerIgn), 0600)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("with multiple hosts with a hostname", func() {
		It("adds the hostname file", func() {
			cluster.Hosts = []*models.Host{
				{
					RequestedHostname: "master0.example.com",
					Role:              models.HostRoleMaster,
				},
				{
					RequestedHostname: "master1.example.com",
					Role:              models.HostRoleMaster,
				},
				{
					RequestedHostname: "worker0.example.com",
					Role:              models.HostRoleWorker,
				},
				{
					RequestedHostname: "worker1.example.com",
					Role:              models.HostRoleWorker,
				},
			}

			// create an ID for each host
			for _, host := range cluster.Hosts {
				id := strfmt.UUID(uuid.New().String())
				host.ID = &id
			}

			g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
				mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

			err := g.createHostIgnitions()
			Expect(err).NotTo(HaveOccurred())

			for _, host := range cluster.Hosts {
				ignBytes, err := ioutil.ReadFile(filepath.Join(workDir, fmt.Sprintf("%s-%s.ign", host.Role, host.ID)))
				Expect(err).NotTo(HaveOccurred())
				config, _, err := config_32.Parse(ignBytes)
				Expect(err).NotTo(HaveOccurred())

				By("Ensuring the correct role file was used")
				sourceURL := config.Ignition.Config.Merge[0].Source
				if host.Role == models.HostRoleMaster {
					Expect(*sourceURL).To(Equal("https://192.168.126.199:22623/config/master"))
				} else if host.Role == models.HostRoleWorker {
					Expect(*sourceURL).To(Equal("https://192.168.126.199:22623/config/worker"))
				}

				By("Validating the hostname file was added")
				var f *config_32_types.File
				for fileidx, file := range config.Storage.Files {
					if file.Node.Path == "/etc/hostname" {
						f = &config.Storage.Files[fileidx]
						break
					}
				}
				Expect(f).NotTo(BeNil())
				Expect(*f.Node.User.Name).To(Equal("root"))
				Expect(*f.FileEmbedded1.Contents.Source).To(Equal(fmt.Sprintf("data:,%s", host.RequestedHostname)))
				Expect(*f.FileEmbedded1.Mode).To(Equal(420))
				Expect(*f.Node.Overwrite).To(Equal(true))
			}
		})
	})

	It("applies overrides correctly", func() {
		hostID := strfmt.UUID(uuid.New().String())
		cluster.Hosts = []*models.Host{{
			ID:                      &hostID,
			RequestedHostname:       "master0.example.com",
			Role:                    models.HostRoleMaster,
			IgnitionConfigOverrides: `{"ignition": {"version": "3.2.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`,
		}}

		g := NewGenerator(workDir, installerCacheDir, cluster, "", "", "", "", nil, log,
			mockOperatorManager, mockProviderRegistry, "").(*installerGenerator)

		err := g.createHostIgnitions()
		Expect(err).NotTo(HaveOccurred())

		ignBytes, err := ioutil.ReadFile(filepath.Join(workDir, fmt.Sprintf("%s-%s.ign", models.HostRoleMaster, hostID)))
		Expect(err).NotTo(HaveOccurred())
		config, _, err := config_32.Parse(ignBytes)
		Expect(err).NotTo(HaveOccurred())

		var exampleFile *config_32_types.File
		var hostnameFile *config_32_types.File
		for fileidx, file := range config.Storage.Files {
			if file.Node.Path == "/tmp/example" {
				exampleFile = &config.Storage.Files[fileidx]
			} else if file.Node.Path == "/etc/hostname" {
				hostnameFile = &config.Storage.Files[fileidx]
			}
		}
		Expect(exampleFile).NotTo(BeNil())
		// check that we didn't overwrite the other files
		Expect(hostnameFile).NotTo(BeNil())

		Expect(*exampleFile.FileEmbedded1.Contents.Source).To(Equal("data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"))
	})
})

var _ = Describe("Openshift cluster ID extraction", func() {
	It("fails on empty ignition file", func() {
		r := ioutil.NopCloser(strings.NewReader(""))
		_, err := ExtractClusterID(r)
		Expect(err.Error()).To(ContainSubstring("not a config (empty)"))
	})

	It("fails on invalid JSON file", func() {
		r := ioutil.NopCloser(strings.NewReader("{"))
		_, err := ExtractClusterID(r)
		Expect(err.Error()).To(ContainSubstring("config is not valid"))
	})

	It("fails on invalid ignition file", func() {
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"invalid.version"}
		}`))
		_, err := ExtractClusterID(r)
		Expect(err.Error()).To(ContainSubstring("unsupported config version"))
	})

	It("fails when there's no CVO file", func() {
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"3.2.0"},
				"storage":{
					"files":[]
				},
				"systemd":{}
		}`))
		_, err := ExtractClusterID(r)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("could not find cvo-overrides file"))
	})

	It("fails when no ClusterID is embedded in cvo-overrides", func() {
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"3.2.0"},
				"storage":{
					"files":[
						{
							"path":"/opt/openshift/manifests/cvo-overrides.yaml",
							"contents":{
								"source":"data:text/plain;charset=utf-8;base64,"
							}
						}
					]
				},
				"systemd":{}
		}`))
		_, err := ExtractClusterID(r)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("no ClusterID field in cvo-overrides file"))
	})

	It("fails when cvo-overrides file cannot be un-marshalled", func() {
		// embedded JSON in the base64 format is "{"
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"3.2.0"},
				"storage":{
					"files":[
						{
							"path":"/opt/openshift/manifests/cvo-overrides.yaml",
							"contents":{
								"source":"data:text/plain;charset=utf-8;base64,ew=="
							}
						}
					]
				},
				"systemd":{}
		}`))
		_, err := ExtractClusterID(r)
		Expect(err).To(Equal(errors.New("yaml: line 1: did not find expected node content")))
	})

	It("is successfull on valid file", func() {
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"3.2.0"},
				"storage":{
					"files":[
						{
							"path":"/opt/openshift/manifests/cvo-overrides.yaml",
							"contents":{
								"source":"data:text/plain;charset=utf-8;base64,YXBpVmVyc2lvbjogY29uZmlnLm9wZW5zaGlmdC5pby92MQpraW5kOiBDbHVzdGVyVmVyc2lvbgptZXRhZGF0YToKICBuYW1lc3BhY2U6IG9wZW5zaGlmdC1jbHVzdGVyLXZlcnNpb24KICBuYW1lOiB2ZXJzaW9uCnNwZWM6CiAgdXBzdHJlYW06IGh0dHBzOi8vYXBpLm9wZW5zaGlmdC5jb20vYXBpL3VwZ3JhZGVzX2luZm8vdjEvZ3JhcGgKICBjaGFubmVsOiBzdGFibGUtNC42CiAgY2x1c3RlcklEOiA0MTk0MGVlOC1lYzk5LTQzZGUtODc2Ni0xNzQzODFiNDkyMWQK"
							}
						}
					]
				},
				"systemd":{}
		}`))
		Expect(ExtractClusterID(r)).To(Equal("41940ee8-ec99-43de-8766-174381b4921d"))
	})

	It("only looks on cvo-overrides file", func() {
		r := ioutil.NopCloser(strings.NewReader(`{
				"ignition":{"version":"3.2.0"},
				"storage":{
					"files":[
						{
							"path":"/opt/openshift/manifests/some-other-file.yaml",
							"contents":{
								"source":"data:text/plain;charset=utf-8;base64,YXBpVmVyc2lvbjogY29uZmlnLm9wZW5zaGlmdC5pby92MQpraW5kOiBDbHVzdGVyVmVyc2lvbgptZXRhZGF0YToKICBuYW1lc3BhY2U6IG9wZW5zaGlmdC1jbHVzdGVyLXZlcnNpb24KICBuYW1lOiB2ZXJzaW9uCnNwZWM6CiAgdXBzdHJlYW06IGh0dHBzOi8vYXBpLm9wZW5zaGlmdC5jb20vYXBpL3VwZ3JhZGVzX2luZm8vdjEvZ3JhcGgKICBjaGFubmVsOiBzdGFibGUtNC42CiAgY2x1c3RlcklEOiA0MTk0MGVlOC1lYzk5LTQzZGUtODc2Ni0xNzQzODFiNDkyMWQK"
							}
						},
						{
							"path":"/opt/openshift/manifests/cvo-overrides.yaml",
							"contents":{
								"source":"data:text/plain;charset=utf-8;base64,"
							}
						}
					]
				},
				"systemd":{}
		}`))
		_, err := ExtractClusterID(r)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("no ClusterID field in cvo-overrides file"))
	})
})

var _ = Describe("Generator UploadToS3", func() {
	var (
		ctx          = context.Background()
		ctrl         *gomock.Controller
		mockS3Client *s3wrapper.MockAPI
	)

	generator := installerGenerator{
		log:     log,
		workDir: workDir,
	}

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		mockS3Client = s3wrapper.NewMockAPI(ctrl)

		generator.s3Client = mockS3Client
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	mockUploadFile := func() *gomock.Call {
		return mockS3Client.EXPECT().UploadFile(gomock.Any(), gomock.Any(), gomock.Any())
	}

	mockUploadObjectTimestamp := func() *gomock.Call {
		return mockS3Client.EXPECT().UpdateObjectTimestamp(gomock.Any(), gomock.Any())
	}

	Context("cluster with known hosts", func() {
		BeforeEach(func() {
			hostID1 := strfmt.UUID(uuid.New().String())
			hostID2 := strfmt.UUID(uuid.New().String())
			cluster.Hosts = []*models.Host{
				{ID: &hostID1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				{ID: &hostID2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
			}
			generator.cluster = cluster
		})

		It("validate upload files names", func() {
			for _, f := range fileNames {
				fullPath := filepath.Join(generator.workDir, f)
				key := filepath.Join(cluster.ID.String(), f)
				mockS3Client.EXPECT().UploadFile(gomock.Any(), fullPath, key).Return(nil).Times(1)
				mockS3Client.EXPECT().UpdateObjectTimestamp(gomock.Any(), key).Return(true, nil).Times(1)
			}
			for i := range cluster.Hosts {
				fullPath := filepath.Join(generator.workDir, hostutil.IgnitionFileName(cluster.Hosts[i]))
				key := filepath.Join(cluster.ID.String(), hostutil.IgnitionFileName(cluster.Hosts[i]))
				mockS3Client.EXPECT().UploadFile(gomock.Any(), fullPath, key).Return(nil).Times(1)
				mockS3Client.EXPECT().UpdateObjectTimestamp(gomock.Any(), key).Return(true, nil).Times(1)
			}

			Expect(generator.UploadToS3(ctx)).Should(Succeed())
		})

		It("upload failure", func() {
			mockUploadFile().Return(nil).Times(1)
			mockUploadObjectTimestamp().Return(true, nil).Times(1)
			mockUploadFile().Return(errors.New("error")).Times(1)

			err := generator.UploadToS3(ctx)
			Expect(err).Should(HaveOccurred())
		})

		It("set timestamp failure", func() {
			mockUploadFile().Return(nil).Times(2)
			mockUploadObjectTimestamp().Return(true, nil).Times(1)
			mockUploadObjectTimestamp().Return(true, errors.New("error")).Times(1)

			err := generator.UploadToS3(ctx)
			Expect(err).Should(HaveOccurred())
		})
	})
})

var _ = Describe("downloadManifest", func() {
	var (
		ctrl         *gomock.Controller
		mockS3Client *s3wrapper.MockAPI
		generator    *installerGenerator
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		mockS3Client = s3wrapper.NewMockAPI(ctrl)
		generator = &installerGenerator{
			log:      log,
			workDir:  workDir,
			s3Client: mockS3Client,
			cluster:  cluster,
		}
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	It("writes the correct file", func() {
		ctx := context.Background()
		manifestName := fmt.Sprintf("%s/manifests/openshift/masters-chrony-configuration.yaml", cluster.ID)
		mockS3Client.EXPECT().Download(ctx, manifestName).Return(ioutil.NopCloser(strings.NewReader("chronyconf")), int64(10), nil)
		Expect(os.Mkdir(filepath.Join(workDir, "/openshift"), 0755)).To(Succeed())
		Expect(os.Mkdir(filepath.Join(workDir, "/manifests"), 0755)).To(Succeed())

		Expect(generator.downloadManifest(ctx, manifestName)).To(Succeed())

		content, err := ioutil.ReadFile(filepath.Join(workDir, "/openshift/masters-chrony-configuration.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(content).To(Equal([]byte("chronyconf")))
	})
})

var _ = Describe("ParseToLatest", func() {
	const v99ignition = `{"ignition": {"version": "9.9.0"},"storage": {"files": []}}`
	const v32ignition = `{"ignition": {"version": "3.2.0"},"storage": {"files": []}}`
	const v31ignition = `{"ignition": {"version": "3.1.0"},"storage": {"files": []}}`
	const v32override = `{"ignition": {"version": "3.2.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
	const v31override = `{"ignition": {"version": "3.1.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
	It("parses a v32 config as 3.2.0", func() {
		config, err := ParseToLatest([]byte(v32ignition))
		Expect(err).ToNot(HaveOccurred())
		Expect(config.Ignition.Version).To(Equal("3.2.0"))

		bytes, err := json.Marshal(config)
		Expect(err).ToNot(HaveOccurred())
		v32Config, _, err := config_32.Parse(bytes)
		Expect(err).ToNot(HaveOccurred())
		Expect(v32Config.Ignition.Version).To(Equal("3.2.0"))
	})

	It("parses a v31 config as 3.1.0", func() {
		config, err := ParseToLatest([]byte(v31ignition))
		Expect(err).ToNot(HaveOccurred())
		Expect(config.Ignition.Version).To(Equal("3.1.0"))

		bytes, err := json.Marshal(config)
		Expect(err).ToNot(HaveOccurred())
		v31Config, _, err := config_31.Parse(bytes)
		Expect(err).ToNot(HaveOccurred())
		Expect(v31Config.Ignition.Version).To(Equal("3.1.0"))
	})

	It("parses a v31 config with v31 override as 3.1.0", func() {
		merge, err := MergeIgnitionConfig([]byte(v31ignition), []byte(v31override))
		Expect(err).ToNot(HaveOccurred())

		config, err := ParseToLatest([]byte(merge))
		Expect(err).ToNot(HaveOccurred())
		Expect(config.Ignition.Version).To(Equal("3.1.0"))

		bytes, err := json.Marshal(config)
		Expect(err).ToNot(HaveOccurred())
		v31Config, _, err := config_31.Parse(bytes)
		Expect(err).ToNot(HaveOccurred())
		Expect(v31Config.Ignition.Version).To(Equal("3.1.0"))
	})

	It("parses a v31 config with v32 override as 3.2.0", func() {
		merge, err := MergeIgnitionConfig([]byte(v31ignition), []byte(v32override))
		Expect(err).ToNot(HaveOccurred())

		config, err := ParseToLatest([]byte(merge))
		Expect(err).ToNot(HaveOccurred())
		Expect(config.Ignition.Version).To(Equal("3.2.0"))

		bytes, err := json.Marshal(config)
		Expect(err).ToNot(HaveOccurred())
		v32Config, _, err := config_32.Parse(bytes)
		Expect(err).ToNot(HaveOccurred())
		Expect(v32Config.Ignition.Version).To(Equal("3.2.0"))
	})

	It("does not parse v99 config", func() {
		_, err := ParseToLatest([]byte(v99ignition))
		Expect(err.Error()).To(ContainSubstring("unsupported config version"))
	})
})

var _ = AfterEach(func() {
	os.RemoveAll("manifests")
})

var _ = Describe("proxySettingsForIgnition", func() {

	Context("test proxy settings in discovery ignition", func() {
		var parameters = []struct {
			httpProxy, httpsProxy, noProxy, res string
		}{
			{"", "", "", ""},
			{
				"http://proxy.proxy", "", "",
				`"proxy": { "httpProxy": "http://proxy.proxy" }`,
			},
			{
				"http://proxy.proxy", "https://proxy.proxy", "",
				`"proxy": { "httpProxy": "http://proxy.proxy", "httpsProxy": "https://proxy.proxy" }`,
			},
			{
				"http://proxy.proxy", "", ".domain",
				`"proxy": { "httpProxy": "http://proxy.proxy", "noProxy": [".domain"] }`,
			},
			{
				"http://proxy.proxy", "https://proxy.proxy", ".domain",
				`"proxy": { "httpProxy": "http://proxy.proxy", "httpsProxy": "https://proxy.proxy", "noProxy": [".domain"] }`,
			},
			{
				"", "https://proxy.proxy", ".domain,123.123.123.123",
				`"proxy": { "httpsProxy": "https://proxy.proxy", "noProxy": [".domain","123.123.123.123"] }`,
			},
			{
				"", "https://proxy.proxy", "",
				`"proxy": { "httpsProxy": "https://proxy.proxy" }`,
			},
			{
				"", "", ".domain", "",
			},
		}

		It("verify rendered proxy settings", func() {
			for _, p := range parameters {
				s, err := proxySettingsForIgnition(p.httpProxy, p.httpsProxy, p.noProxy)
				Expect(err).To(BeNil())
				Expect(s).To(Equal(p.res))
			}
		})
	})
})

var _ = Describe("IgnitionBuilder", func() {
	var (
		ctrl                              *gomock.Controller
		infraEnv                          common.InfraEnv
		log                               logrus.FieldLogger
		builder                           IgnitionBuilder
		mockStaticNetworkConfig           *staticnetworkconfig.MockStaticNetworkConfig
		mockMirrorRegistriesConfigBuilder *mirrorregistries.MockMirrorRegistriesConfigBuilder
		infraEnvID                        strfmt.UUID
	)

	BeforeEach(func() {
		log = common.GetTestLog()
		infraEnvID = strfmt.UUID("a640ef36-dcb1-11ea-87d0-0242ac130003")
		ctrl = gomock.NewController(GinkgoT())
		mockStaticNetworkConfig = staticnetworkconfig.NewMockStaticNetworkConfig(ctrl)
		mockMirrorRegistriesConfigBuilder = mirrorregistries.NewMockMirrorRegistriesConfigBuilder(ctrl)
		infraEnv = common.InfraEnv{InfraEnv: models.InfraEnv{
			ID:            &infraEnvID,
			PullSecretSet: false,
		}, PullSecret: "{\"auths\":{\"cloud.openshift.com\":{\"auth\":\"dG9rZW46dGVzdAo=\",\"email\":\"coyote@acme.com\"}}}"}
		//cluster.ImageInfo = &models.ImageInfo{}
		builder = NewBuilder(log, mockStaticNetworkConfig, mockMirrorRegistriesConfigBuilder)
	})

	Context("with auth enabled", func() {

		It("ignition_file_fails_missing_Pull_Secret_token", func() {
			infraEnvID = strfmt.UUID("a640ef36-dcb1-11ea-87d0-0242ac130003")
			infraEnvWithoutToken := common.InfraEnv{InfraEnv: models.InfraEnv{
				ID:            &infraEnvID,
				PullSecretSet: false,
			}, PullSecret: "{\"auths\":{\"registry.redhat.com\":{\"auth\":\"dG9rZW46dGVzdAo=\",\"email\":\"coyote@acme.com\"}}}"}

			_, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnvWithoutToken, IgnitionConfig{}, false, auth.TypeRHSSO)

			Expect(err).ShouldNot(BeNil())
		})

		It("ignition_file_contains_pull_secret_token", func() {
			mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
			text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)

			Expect(err).Should(BeNil())
			Expect(text).Should(ContainSubstring("PULL_SECRET_TOKEN"))
		})
	})

	It("auth_disabled_no_pull_secret_token", func() {
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeNone)

		Expect(err).Should(BeNil())
		Expect(text).ShouldNot(ContainSubstring("PULL_SECRET_TOKEN"))
	})

	It("ignition_file_contains_url", func() {
		serviceBaseURL := "file://10.56.20.70:7878"
		config := IgnitionConfig{ServiceBaseURL: serviceBaseURL}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring(fmt.Sprintf("--url %s", serviceBaseURL)))
	})

	It("ignition_file_safe_for_logging", func() {
		serviceBaseURL := "file://10.56.20.70:7878"
		config := IgnitionConfig{ServiceBaseURL: serviceBaseURL}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, true, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).ShouldNot(ContainSubstring("cloud.openshift.com"))
		Expect(text).Should(ContainSubstring("data:,*****"))
	})

	It("enabled_cert_verification", func() {
		config := IgnitionConfig{SkipCertVerification: false}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring("--insecure=false"))
	})

	It("disabled_cert_verification", func() {
		config := IgnitionConfig{SkipCertVerification: true}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring("--insecure=true"))
	})

	It("cert_verification_enabled_by_default", func() {
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring("--insecure=false"))
	})

	It("ignition_file_contains_http_proxy", func() {
		proxy := models.Proxy{
			HTTPProxy: swag.String("http://10.10.1.1:3128"),
			NoProxy:   swag.String("quay.io"),
		}
		infraEnv.Proxy = &proxy
		//cluster.HTTPProxy = "http://10.10.1.1:3128"
		//cluster.NoProxy = "quay.io"
		serviceBaseURL := "file://10.56.20.70:7878"
		config := IgnitionConfig{ServiceBaseURL: serviceBaseURL}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring(`"proxy": { "httpProxy": "http://10.10.1.1:3128", "noProxy": ["quay.io"] }`))
	})

	It("ignition_file_contains_asterisk_no_proxy", func() {
		proxy := models.Proxy{
			HTTPProxy: swag.String("http://10.10.1.1:3128"),
			NoProxy:   swag.String("*"),
		}
		infraEnv.Proxy = &proxy
		//cluster.HTTPProxy = "http://10.10.1.1:3128"
		//cluster.NoProxy = "*"
		serviceBaseURL := "file://10.56.20.70:7878"
		config := IgnitionConfig{ServiceBaseURL: serviceBaseURL}
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, config, false, auth.TypeRHSSO)

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring(`"proxy": { "httpProxy": "http://10.10.1.1:3128", "noProxy": ["*"] }`))
	})

	It("produces a valid ignition v3.1 spec by default", func() {
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())

		config, report, err := config_31.Parse([]byte(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.IsFatal()).To(BeFalse())
		Expect(config.Ignition.Version).To(Equal("3.1.0"))
	})

	// TODO(deprecate-ignition-3.1.0)
	It("produces a valid ignition v3.1 spec with overrides", func() {
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())

		config, report, err := config_31.Parse([]byte(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.IsFatal()).To(BeFalse())
		numOfFiles := len(config.Storage.Files)

		infraEnv.IgnitionConfigOverride = `{"ignition": {"version": "3.1.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err = builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())

		config, report, err = config_31.Parse([]byte(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.IsFatal()).To(BeFalse())
		Expect(config.Ignition.Version).To(Equal("3.1.0"))
		Expect(len(config.Storage.Files)).To(Equal(numOfFiles + 1))
	})

	It("produces a valid ignition spec with v3.2 overrides", func() {
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())

		config, report, err := config_31.Parse([]byte(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.IsFatal()).To(BeFalse())
		Expect(config.Ignition.Version).To(Equal("3.1.0"))
		numOfFiles := len(config.Storage.Files)

		infraEnv.IgnitionConfigOverride = `{"ignition": {"version": "3.2.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		text, err = builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())

		config2, report, err := config_32.Parse([]byte(text))
		Expect(err).NotTo(HaveOccurred())
		Expect(report.IsFatal()).To(BeFalse())
		Expect(config2.Ignition.Version).To(Equal("3.2.0"))
		Expect(len(config2.Storage.Files)).To(Equal(numOfFiles + 1))
	})

	It("fails when given overrides with an incompatible version", func() {
		infraEnv.IgnitionConfigOverride = `{"ignition": {"version": "2.2.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
		_, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)

		Expect(err).To(HaveOccurred())
	})

	It("applies day2 overrides successfuly", func() {
		hostID := strfmt.UUID(uuid.New().String())
		cluster.Hosts = []*models.Host{{
			ID:                      &hostID,
			RequestedHostname:       "day2worker.example.com",
			Role:                    models.HostRoleWorker,
			IgnitionConfigOverrides: `{"ignition": {"version": "3.2.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`,
		}}
		serviceBaseURL := "http://10.56.20.70:7878"

		text, err := builder.FormatSecondDayWorkerIgnitionFile(serviceBaseURL, nil, "", cluster.Hosts[0])

		Expect(err).Should(BeNil())
		Expect(text).Should(ContainSubstring("/tmp/example"))
	})

	Context("static network config", func() {
		formattedInput := "some formated input"
		staticnetworkConfigOutput := []staticnetworkconfig.StaticNetworkConfigData{
			{
				FilePath:     "nic10.nmconnection",
				FileContents: "nic10 nmconnection content",
			},
			{
				FilePath:     "nic20.nmconnection",
				FileContents: "nic10 nmconnection content",
			},
			{
				FilePath:     "mac_interface.ini",
				FileContents: "nic10=mac10\nnic20=mac20",
			},
		}

		It("produces a valid ignition v3.1 spec with static ips paramters", func() {
			mockStaticNetworkConfig.EXPECT().GenerateStaticNetworkConfigData(gomock.Any(), formattedInput).Return(staticnetworkConfigOutput, nil).Times(1)
			infraEnv.StaticNetworkConfig = formattedInput
			infraEnv.Type = common.ImageTypePtr(models.ImageTypeFullIso)
			mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
			text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
			Expect(err).NotTo(HaveOccurred())
			config, report, err := config_31.Parse([]byte(text))
			Expect(err).NotTo(HaveOccurred())
			Expect(report.IsFatal()).To(BeFalse())
			count := 0
			for _, f := range config.Storage.Files {
				if strings.HasSuffix(f.Path, "nmconnection") || strings.HasSuffix(f.Path, "mac_interface.ini") {
					count += 1
				}
			}
			Expect(count).Should(Equal(3))
		})
		It("Doesn't include static network config for minimal isos", func() {
			mockStaticNetworkConfig.EXPECT().GenerateStaticNetworkConfigData(gomock.Any(), formattedInput).Return(staticnetworkConfigOutput, nil).Times(1)
			infraEnv.StaticNetworkConfig = formattedInput
			infraEnv.Type = common.ImageTypePtr(models.ImageTypeMinimalIso)
			mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
			text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
			Expect(err).NotTo(HaveOccurred())
			config, report, err := config_31.Parse([]byte(text))
			Expect(err).NotTo(HaveOccurred())
			Expect(report.IsFatal()).To(BeFalse())
			count := 0
			for _, f := range config.Storage.Files {
				if strings.HasSuffix(f.Path, "nmconnection") || strings.HasSuffix(f.Path, "mac_interface.ini") || strings.HasSuffix(f.Path, "02-hostname-mode.conf") {
					count += 1
				}
			}
			Expect(count).Should(Equal(0))
		})
	})

	Context("mirror registries config", func() {

		It("produce ignition with mirror registries config", func() {
			mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(true).Times(1)
			mockMirrorRegistriesConfigBuilder.EXPECT().GetMirrorCA().Return([]byte("some ca config"), nil).Times(1)
			mockMirrorRegistriesConfigBuilder.EXPECT().GetMirrorRegistries().Return([]byte("some mirror registries config"), nil).Times(1)
			text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
			Expect(err).NotTo(HaveOccurred())
			config, report, err := config_31.Parse([]byte(text))
			Expect(err).NotTo(HaveOccurred())
			Expect(report.IsFatal()).To(BeFalse())
			count := 0
			for _, f := range config.Storage.Files {
				if strings.HasSuffix(f.Path, "registries.conf") || strings.HasSuffix(f.Path, "domain.crt") {
					count += 1
				}
			}
			Expect(count).Should(Equal(2))
		})
	})
})

var _ = Describe("Ignition SSH key building", func() {
	var (
		ctrl                              *gomock.Controller
		infraEnv                          common.InfraEnv
		builder                           IgnitionBuilder
		mockStaticNetworkConfig           *staticnetworkconfig.MockStaticNetworkConfig
		mockMirrorRegistriesConfigBuilder *mirrorregistries.MockMirrorRegistriesConfigBuilder
		infraEnvID                        strfmt.UUID
	)
	buildIgnitionAndAssertSubString := func(SSHPublicKey string, shouldExist bool, subStr string) {
		infraEnv.SSHAuthorizedKey = SSHPublicKey
		text, err := builder.FormatDiscoveryIgnitionFile(context.Background(), &infraEnv, IgnitionConfig{}, false, auth.TypeRHSSO)
		Expect(err).NotTo(HaveOccurred())
		if shouldExist {
			Expect(text).Should(ContainSubstring(subStr))
		} else {
			Expect(text).ShouldNot(ContainSubstring(subStr))
		}
	}
	BeforeEach(func() {
		infraEnvID = strfmt.UUID("a64fff36-dcb1-11ea-87d0-0242ac130003")
		ctrl = gomock.NewController(GinkgoT())
		mockStaticNetworkConfig = staticnetworkconfig.NewMockStaticNetworkConfig(ctrl)
		mockMirrorRegistriesConfigBuilder = mirrorregistries.NewMockMirrorRegistriesConfigBuilder(ctrl)
		infraEnv = common.InfraEnv{
			InfraEnv: models.InfraEnv{
				ID:            &infraEnvID,
				PullSecretSet: false,
			},
			PullSecret: "{\"auths\":{\"cloud.openshift.com\":{\"auth\":\"dG9rZW46dGVzdAo=\",\"email\":\"coyote@acme.com\"}}}",
		}
		//cluster.ImageInfo = &models.ImageInfo{}
		builder = NewBuilder(log, mockStaticNetworkConfig, mockMirrorRegistriesConfigBuilder)
		mockMirrorRegistriesConfigBuilder.EXPECT().IsMirrorRegistriesConfigured().Return(false).Times(1)
	})
	Context("when empty or invalid input", func() {
		It("white_space_string should return an empty string", func() {
			buildIgnitionAndAssertSubString("  \n  \n \t \n  ", false, "sshAuthorizedKeys")
		})
		It("Empty string should return an empty string", func() {
			buildIgnitionAndAssertSubString("", false, "sshAuthorizedKeys")
		})
	})
	Context("when ssh key exists, escape when needed", func() {
		It("Single key without needed escaping", func() {
			buildIgnitionAndAssertSubString("ssh-rsa key coyote@acme.com", true, `"sshAuthorizedKeys":["ssh-rsa key coyote@acme.com"]`)
		})
		It("Multiple keys without needed escaping", func() {
			buildIgnitionAndAssertSubString("ssh-rsa key coyote@acme.com\nssh-rsa key2 coyote@acme.com",
				true,
				`"sshAuthorizedKeys":["ssh-rsa key coyote@acme.com","ssh-rsa key2 coyote@acme.com"]`)
		})
		It("Single key with escaping", func() {
			buildIgnitionAndAssertSubString(`ssh-rsa key coyote\123@acme.com`, true, `"sshAuthorizedKeys":["ssh-rsa key coyote\\123@acme.com"]`)
		})
		It("Multiple keys with escaping", func() {
			buildIgnitionAndAssertSubString(`ssh-rsa key coyote\123@acme.com
			ssh-rsa key2 coyote@acme.com`,
				true,
				`"sshAuthorizedKeys":["ssh-rsa key coyote\\123@acme.com","ssh-rsa key2 coyote@acme.com"]`)
		})
		It("Multiple keys with escaping and white space", func() {
			buildIgnitionAndAssertSubString(`
			ssh-rsa key coyote\123@acme.com

			ssh-rsa key2 c\0899oyote@acme.com
			`, true, `"sshAuthorizedKeys":["ssh-rsa key coyote\\123@acme.com","ssh-rsa key2 c\\0899oyote@acme.com"]`)
		})
	})
})

var _ = Describe("FormatSecondDayWorkerIgnitionFile", func() {

	var (
		ctrl                              *gomock.Controller
		log                               logrus.FieldLogger
		builder                           IgnitionBuilder
		mockStaticNetworkConfig           *staticnetworkconfig.MockStaticNetworkConfig
		mockMirrorRegistriesConfigBuilder *mirrorregistries.MockMirrorRegistriesConfigBuilder
		mockHost                          *models.Host
	)

	BeforeEach(func() {
		log = common.GetTestLog()
		ctrl = gomock.NewController(GinkgoT())
		mockStaticNetworkConfig = staticnetworkconfig.NewMockStaticNetworkConfig(ctrl)
		mockMirrorRegistriesConfigBuilder = mirrorregistries.NewMockMirrorRegistriesConfigBuilder(ctrl)
		mockHost = &models.Host{Inventory: hostInventory}
		builder = NewBuilder(log, mockStaticNetworkConfig, mockMirrorRegistriesConfigBuilder)
	})

	Context("test custom ignition endpoint", func() {

		It("are rendered properly without ca cert and token", func() {
			ign, err := builder.FormatSecondDayWorkerIgnitionFile("http://url.com", nil, "", mockHost)
			Expect(err).NotTo(HaveOccurred())

			ignConfig, _, err := config_31.Parse(ign)
			Expect(err).NotTo(HaveOccurred())
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].Source)).Should(Equal("http://url.com"))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders).Should(HaveLen(0))
			Expect(ignConfig.Ignition.Security.TLS.CertificateAuthorities).Should(HaveLen(0))
		})

		It("are rendered properly with token", func() {
			token := "xyzabc123"
			ign, err := builder.FormatSecondDayWorkerIgnitionFile("http://url.com", nil, token, mockHost)
			Expect(err).NotTo(HaveOccurred())

			ignConfig, _, err := config_31.Parse(ign)
			Expect(err).NotTo(HaveOccurred())
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].Source)).Should(Equal("http://url.com"))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders).Should(HaveLen(1))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders[0].Name).Should(Equal("Authorization"))
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].HTTPHeaders[0].Value)).Should(Equal("Bearer " + token))
			Expect(ignConfig.Ignition.Security.TLS.CertificateAuthorities).Should(HaveLen(0))
		})

		It("are rendered properly with ca cert", func() {
			ca := "-----BEGIN CERTIFICATE-----\nMIIDozCCAougAwIBAgIULCOqWTF" +
				"aEA8gNEmV+rb7h1v0r3EwDQYJKoZIhvcNAQELBQAwYTELMAkGA1UEBhMCaXMxCzAJBgNVBAgMAmRk" +
				"2lyDI6UR3Fbz4pVVAxGXnVhBExjBE=\n-----END CERTIFICATE-----"
			encodedCa := base64.StdEncoding.EncodeToString([]byte(ca))
			ign, err := builder.FormatSecondDayWorkerIgnitionFile("https://url.com", &encodedCa, "", mockHost)
			Expect(err).NotTo(HaveOccurred())

			ignConfig, _, err := config_31.Parse(ign)
			Expect(err).NotTo(HaveOccurred())
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].Source)).Should(Equal("https://url.com"))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders).Should(HaveLen(0))
			Expect(ignConfig.Ignition.Security.TLS.CertificateAuthorities).Should(HaveLen(1))
			Expect(swag.StringValue(ignConfig.Ignition.Security.TLS.CertificateAuthorities[0].Source)).Should(Equal("data:text/plain;base64," + encodedCa))
		})

		It("are rendered properly with ca cert and token", func() {
			token := "xyzabc123"
			ca := "-----BEGIN CERTIFICATE-----\nMIIDozCCAougAwIBAgIULCOqWTF" +
				"aEA8gNEmV+rb7h1v0r3EwDQYJKoZIhvcNAQELBQAwYTELMAkGA1UEBhMCaXMxCzAJBgNVBAgMAmRk" +
				"2lyDI6UR3Fbz4pVVAxGXnVhBExjBE=\n-----END CERTIFICATE-----"
			encodedCa := base64.StdEncoding.EncodeToString([]byte(ca))
			ign, err := builder.FormatSecondDayWorkerIgnitionFile("https://url.com", &encodedCa, token, mockHost)

			Expect(err).NotTo(HaveOccurred())

			ignConfig, _, err := config_31.Parse(ign)
			Expect(err).NotTo(HaveOccurred())
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].Source)).Should(Equal("https://url.com"))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders).Should(HaveLen(1))
			Expect(ignConfig.Ignition.Config.Merge[0].HTTPHeaders[0].Name).Should(Equal("Authorization"))
			Expect(swag.StringValue(ignConfig.Ignition.Config.Merge[0].HTTPHeaders[0].Value)).Should(Equal("Bearer " + token))
			Expect(ignConfig.Ignition.Security.TLS.CertificateAuthorities).Should(HaveLen(1))
			Expect(swag.StringValue(ignConfig.Ignition.Security.TLS.CertificateAuthorities[0].Source)).Should(Equal("data:text/plain;base64," + encodedCa))
		})
	})
})
