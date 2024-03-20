
## Steps

### Initialize the project

```
mkdir marklogic-operator
cd marklogic-operator
operator-sdk init --domain marklogic.com --repo github.com/example/marklogic-operator
```

### Create an API

```
operator-sdk create api --group operator --version v1alpha1 --kind MarklogicGroup --resource --controller
```

