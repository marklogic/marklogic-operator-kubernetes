# MarkLogic Kubernetes Operator

## Introduction

The MarkLogic Kubernetes Operator is a tool that allows you to deploy and manage MarkLogic clusters on Kubernetes. It provides a declarative way to define and manage MarkLogic resources such as clusters, databases, and forests.

## Code Structure
api/ Contains the API definition
controllers/ Contains the code for controllers
config/ Contains configuration files to launch the project on a cluster
config/samples/ Contains the samples to create marklogic cluster using the Operator.
test/ Contains E2E tests for the operator
pkg/ Contains golang packges to support reconciliation, utilities.

## Getting Started

make build: build the project
make run: run the operator locally
