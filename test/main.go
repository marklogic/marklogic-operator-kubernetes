package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
)

func substituteValues(templateContent string) string {
	mapperfunc := func(IMAGE_NAME string) string {
		return os.Getenv(IMAGE_NAME)
	}
	return os.Expand(templateContent, mapperfunc)
}

func preprocessFiles(files []string) error {
	for _, file := range files {
		fmt.Printf("Processing file %s", file)
		content, err := ioutil.ReadFile(file)
		if err != nil {
			return err
		}
		substituted := substituteValues(string(content))
		err = ioutil.WriteFile(file, []byte(substituted), 0644)
		if err != nil {
			return err
		}
		fmt.Print(string(substituted))
	}
	return nil
}

func runKUTTLTests() error {
	cmd := exec.Command("kubectl", "kuttl", "test", "--config", "./kuttl-test.yaml")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	yamlFiles := []string{"./test/kuttl-tests/e2e/00-assert-create.yaml", "./test/kuttl-tests/e2e/00-install-create.yaml"}

	err := preprocessFiles(yamlFiles)
	if err != nil {
		fmt.Printf("Error preprocessing test files: %s\n", err)
		os.Exit(1)
	}

	err = runKUTTLTests()
	if err != nil {
		fmt.Printf("Error running KUTTL tests: %s\n", err)
		os.Exit(1)
	}

	fmt.Println("KUTTL tests completed successfully")
}
