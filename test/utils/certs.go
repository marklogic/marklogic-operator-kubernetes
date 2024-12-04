package utils

import (
	"fmt"
	"os/exec"
	"strings"

	// e2eutils "sigs.k8s.io/e2e-framework/support/utils"
)

func GenerateCertificates(path string, caPath string) error {
	var err error
	fmt.Println("====Generating TLS Certificates")
	geeTlsKeyCmd := strings.Replace("openssl genpkey -algorithm RSA -out path/tls.key", "path", path, -1)
	genCsrCmd := strings.Replace("openssl req -new -key path/tls.key -config path/server.cnf -out path/tls.csr", "path", path, -1)
	genCrtCmd := strings.Replace(strings.Replace("openssl x509 -req -CA caPath/cacert.pem -CAkey caPath/ca-private-key.pem -CAcreateserial -CAserial path/cacert.srl -in path/tls.csr -out path/tls.crt -days 365", "path", path, -1), "caPath", caPath, -1)
	rvariable := []string{geeTlsKeyCmd, genCsrCmd, genCrtCmd}
	for _, j := range rvariable {
		cmd := exec.Command("bash", "-c", j)
		err = cmd.Run()
	}
	return err
}

func GenerateCACertificate(caPath string) error {
	var err error
	fmt.Println("====Generating CA Certificates")
	genKeyCmd := strings.Replace("openssl genrsa -out caPath/ca-private-key.pem 2048", "caPath", caPath, -1)
	genCACertCmd := strings.Replace("openssl req -new -x509 -days 3650 -key caPath/ca-private-key.pem -out caPath/cacert.pem -subj '/CN=TlsTest/C=US/ST=California/L=RedwoodCity/O=Progress/OU=MarkLogic'", "caPath", caPath, -1)
	pwdCMD := "pwd"
	rvariable := []string{pwdCMD, genKeyCmd, genCACertCmd}
	for _, j := range rvariable {
		fmt.Println(j)
		cmd := exec.Command("bash", "-c", j)
		err = cmd.Run()
		if err != nil {
			fmt.Println(err)
		}
	}
	return err
}