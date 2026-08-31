// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

/* groovylint-disable CompileStatic, LineLength, VariableTypeRequired */
// This Jenkinsfile defines internal MarkLogic build pipeline.

//Shared library definitions: https://github.com/marklogic/MarkLogic-Build-Libs/tree/1.0-declarative/vars
@Library('shared-libraries@1.0-declarative')
import groovy.json.JsonSlurperClassic

emailList = 'vitaly.korolev@progress.com, sumanth.ravipati@progress.com, peng.zhou@progress.com, barkha.choithani@progress.com, romain.winieski@progress.com'
emailSecList = 'Mahalakshmi.Srinivasan@progress.com'
gitCredID = 'marklogic-builder-github'
operatorRegistry = 'ml-marklogic-operator-dev.bed-artifactory.bedford.progress.com'
JIRA_ID = ''
JIRA_ID_PATTERN = /(?i)(MLE)-\d{3,6}/
operatorRepo = 'marklogic-kubernetes-operator'
timeStamp = new Date().format('yyyyMMdd')
branchNameTag = env.BRANCH_NAME.replaceAll('/', '-')

// Define local funtions
void preBuildCheck() {
    // Initialize parameters as env variables as workaround for https://issues.jenkins-ci.org/browse/JENKINS-41929
    evaluate """${ def script = ''; params.each { k, v -> script += "env.${k } = '''${v}'''\n" }; return script}"""

    JIRA_ID = extractJiraID()
    echo 'Jira ticket number: ' + JIRA_ID

    if (env.GIT_URL) {
        githubAPIUrl = GIT_URL.replace('.git', '').replace('github.com', 'api.github.com/repos')
        echo 'githubAPIUrl: ' + githubAPIUrl
    } else {
        echo 'Warning: GIT_URL is not defined'
    }

    if (env.CHANGE_ID) {
        if (prDraftCheck()) { sh 'exit 1' }
        if (getReviewState().equalsIgnoreCase('CHANGES_REQUESTED')) {
            echo 'PR changes requested. (' + reviewState + ') Aborting.'
            sh 'exit 1'
        }
    }

    // our VMs sometimes disable bridge traffic. this should help to restore it.
    sh 'sudo modprobe br_netfilter'
    sh 'sudo sh -c "echo 1 > /proc/sys/net/bridge/bridge-nf-call-iptables"'
}

@NonCPS
def extractJiraID() {
    // Extract Jira ID from one of the environment variables
    def match
    if (env.CHANGE_TITLE) {
        match = env.CHANGE_TITLE =~ JIRA_ID_PATTERN
    }
    else if (env.BRANCH_NAME) {
        match = env.BRANCH_NAME =~ JIRA_ID_PATTERN
    }
    else if (env.GIT_BRANCH) {
        match = env.GIT_BRANCH =~ JIRA_ID_PATTERN
    }
    else {
        echo 'Warning: No Git title or branch available.'
        return ''
    }
    try {
        return match[0][0]
    } catch (any) {
        echo 'Warning: Jira ticket number not detected.'
        return ''
    }
}

def prDraftCheck() {
    withCredentials([usernameColonPassword(credentialsId: gitCredID, variable: 'Credentials')]) {
        PrObj = sh(returnStdout: true, script:'''
                    curl -s -u $Credentials  -X GET  ''' + githubAPIUrl + '''/pulls/$CHANGE_ID
                    ''')
    }
    def jsonObj = new JsonSlurperClassic().parseText(PrObj.toString().trim())
    return jsonObj.draft
}

def getReviewState() {
    def reviewResponse
    def commitHash
    withCredentials([usernameColonPassword(credentialsId: gitCredID, variable: 'Credentials')]) {
        reviewResponse = sh(returnStdout: true, script:'''
                            curl -s -u $Credentials  -X GET  ''' + githubAPIUrl + '''/pulls/$CHANGE_ID/reviews
                            ''')
        commitHash = sh(returnStdout: true, script:'''
                        curl -s -u $Credentials  -X GET  ''' + githubAPIUrl + '''/pulls/$CHANGE_ID
                        ''')
    }
    def jsonObj = new JsonSlurperClassic().parseText(commitHash.toString().trim())
    def commitId = jsonObj.head.sha
    println(commitId)
    def reviewState = getReviewStateOfPR reviewResponse, 2, commitId
    echo reviewState
    return reviewState
}

void resultNotification(status) {
    def author, authorEmail, emailList
    //add author of a PR to email list if available
    if (env.CHANGE_AUTHOR) {
        author = env.CHANGE_AUTHOR.toString().trim().toLowerCase()
        authorEmail = getEmailFromGITUser author
        emailList = params.emailList + ',' + authorEmail
    } else {
        emailList = params.emailList
    }
    jira_link = "https://progresssoftware.atlassian.net/browse/${JIRA_ID}"
    email_body = "<b>Jenkins pipeline for</b> ${env.JOB_NAME} <br><b>Build Number: </b>${env.BUILD_NUMBER} <br><br><b>Build URL: </b><br><a href='${env.BUILD_URL}'>${env.BUILD_URL}</a>"
    jira_email_body = "${email_body} <br><br><b>Jira URL: </b><br><a href='${jira_link}'>${jira_link}</a>"

    if (JIRA_ID) {
        def comment = [ body: "Jenkins pipeline build result: ${status}" ]
        jiraAddComment site: 'JIRA', idOrKey: JIRA_ID, failOnError: false, input: comment
        mail charset: 'UTF-8', mimeType: 'text/html', to: "${emailList}", body: "${jira_email_body}", subject: "🥷 ${status}: ${env.JOB_NAME} #${env.BUILD_NUMBER} - ${JIRA_ID}"
    } else {
        mail charset: 'UTF-8', mimeType: 'text/html', to: "${emailList}", body: "${email_body}", subject: "🥷 ${status}: ${env.JOB_NAME} #${env.BUILD_NUMBER}"
    }
}

void publishTestResults() {
    junit allowEmptyResults:true, testResults: '**/test/test_results/*.xml'
    archiveArtifacts artifacts: '**/test/test_results/*.xml', allowEmptyArchive: true
}

void runTests() {
    sh "make test"
}

void runMinikubeSetup(String profile = 'minikube', boolean reuse = false) {
    sh """
        make e2e-setup-minikube IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${profile} MINIKUBE_REUSE=${reuse}
    """
}

void runE2eTests(String scope = 'cluster', String installMode = 'fresh', String profile = 'minikube') {
    if (!(installMode in ['fresh', 'upgrade'])) {
        error "Unsupported E2E install mode '${installMode}'. Supported modes: fresh, upgrade"
    }

    if (installMode == 'upgrade') {
        if (scope != 'cluster') {
            error "Upgrade e2e flow only supports the cluster suite target."
        }
        sh """
            make e2e-test-upgrade-cluster IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${profile}
        """
        return
    }

    if (!(scope in ['cluster', 'dynamic-host', 'volume-resize'])) {
        error "Unsupported E2E scope '${scope}'. Supported scopes: cluster, dynamic-host, volume-resize"
    }
    sh """
        make e2e-test-${scope} IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${profile}
    """
}

void runMinikubeCleanup(String profile = 'minikube', boolean reuse = false) {
    sh """
        make e2e-cleanup-minikube MINIKUBE_PROFILE=${profile} MINIKUBE_REUSE=${reuse}
    """
}

// Istio ambient mode is now installed by `make e2e-setup-minikube` itself (see Makefile),
// so Istio e2e tests reuse the same minikube cluster as the rest of the suite instead of
// requiring a dedicated cluster/shard to be spun up and torn down.
void runIstioE2eTests(String profile = 'minikube') {
    sh """
        make e2e-test-istio IMG=${operatorRepo}:${VERSION} E2E_ISTIO_AMBIENT=true MINIKUBE_PROFILE=${profile}
    """
}

// ---------------------------------------------------------------------------
// EKS / ECR helper functions
// AWS credentials are bound using the 'KUBE_NINJAS_OPS_AWS_JENKINS' credential ID.
// AWS_ACCOUNT_ID is resolved via 'aws sts get-caller-identity' inside withEksCredentials.
// EKS_MARKLOGIC_IMAGE_VERSION is derived at runtime from AWS_ACCOUNT_ID + EKS_MARKLOGIC_IMAGE_TAG.
// ---------------------------------------------------------------------------

void withEksCredentials(Closure body) {
    withCredentials([[$class: 'AmazonWebServicesCredentialsBinding',
                      credentialsId: 'KUBE_NINJAS_OPS_AWS_JENKINS',
                      accessKeyVariable: 'AWS_ACCESS_KEY_ID',
                      secretKeyVariable: 'AWS_SECRET_ACCESS_KEY']]) {
        // Resolve account ID via STS — no account number is hardcoded in this file.
        env.AWS_ACCOUNT_ID = sh(returnStdout: true,
            script: 'aws sts get-caller-identity --query Account --output text').trim()
        // Construct the ECR image URL; tag is configurable via the EKS_MARKLOGIC_IMAGE_TAG parameter.
        env.EKS_MARKLOGIC_IMAGE_VERSION = "${env.AWS_ACCOUNT_ID}.dkr.ecr.us-west-1.amazonaws.com/jenkins-kube-ninjas/marklogic-server-ubi-rootless:${params.EKS_MARKLOGIC_IMAGE_TAG}"
        body()
    }
}

void runEKSSetup() {
    withEksCredentials {
        sh """
            make e2e-setup-eks \\
              E2E_MARKLOGIC_IMAGE_VERSION=${env.EKS_MARKLOGIC_IMAGE_VERSION}
        """
    }
}

void runEKSE2eTests() {
    withEksCredentials {
        sh """
            make e2e-test-eks \\
              E2E_MARKLOGIC_IMAGE_VERSION=${env.EKS_MARKLOGIC_IMAGE_VERSION}
        """
    }
}

void runEKSCleanup() {
    withEksCredentials {
        sh 'make e2e-cleanup-eks'
    }
}

void runEKSIstioSetup() {
    withEksCredentials {
        sh """
            make e2e-setup-eks-istio \\
              E2E_MARKLOGIC_IMAGE_VERSION=${env.EKS_MARKLOGIC_IMAGE_VERSION}
        """
    }
}

void runEKSIstioE2eTests() {
    withEksCredentials {
        sh """
            make e2e-test-eks-istio \\
              E2E_MARKLOGIC_IMAGE_VERSION=${env.EKS_MARKLOGIC_IMAGE_VERSION}
        """
    }
}

void runHelmNamespaceScopedE2eTests(String installMode = 'fresh', String profile = 'minikube') {
    if (!(installMode in ['fresh', 'upgrade'])) {
        error "Unsupported E2E install mode '${installMode}'. Supported modes: fresh, upgrade"
    }

    if (installMode == 'upgrade') {
        sh """
            make e2e-test-upgrade-helm-namespace IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${profile}
        """
        return
    }

    sh """
        make e2e-test-helm-namespace IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${profile}
    """
}

// Dynamically extracts dependent container image references from their canonical
// sources and triggers the BlackDuck scan job with the full CONTAINER_IMAGES list.
// PUBLISH_IMAGE=true also prepends the published operator registry image.
void runBlackDuckScan() {
    def fluentBitImage = sh(returnStdout: true, script: "grep -E '^export FLUENT_BIT_IMAGE' Makefile | cut -d'=' -f2 | tr -d ' '").trim()
    def haProxyImage   = sh(returnStdout: true, script: "grep -oE 'haproxytech/haproxy-alpine:[0-9.]+' Makefile | head -1").trim()
    def ubi9Image      = sh(returnStdout: true, script: "grep -oE 'redhat/ubi9:[0-9.]+' pkg/k8sutil/statefulset.go | head -1").trim()

    if (!fluentBitImage) { error "runBlackDuckScan: could not resolve FLUENT_BIT_IMAGE from Makefile" }
    if (!haProxyImage)   { error "runBlackDuckScan: could not resolve HAProxy image from Makefile" }
    if (!ubi9Image)      { error "runBlackDuckScan: could not resolve UBI9 image from pkg/k8sutil/statefulset.go" }

    def dependentImages = "${fluentBitImage},${haProxyImage},${ubi9Image}"

    def containerImages
    if (params.PUBLISH_IMAGE) {
        containerImages = "${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}-${timeStamp},${dependentImages}"
    } else {
        containerImages = dependentImages
    }

    build job: 'securityscans/Blackduck/KubeNinjas/kubernetes-operator',
          wait: false,
          parameters: [
              string(name: 'branch',           value: "${env.BRANCH_NAME}"),
              string(name: 'CONTAINER_IMAGES', value: containerImages)
          ]
}

/**
 * Publishes the built Docker image to the internal Artifactory registry.
 * Tags the image with multiple tags (version-specific, branch-specific, latest).
 * Requires Artifactory credentials.
 */
void publishToInternalRegistry() {
    withCredentials([usernamePassword(credentialsId: 'builder-credentials-artifactory', passwordVariable: 'docker_password', usernameVariable: 'docker_user')]) {
        
        sh """
            # make sure to logout first to avoid issues with cached credentials
            docker logout ${operatorRegistry}
            echo "${docker_password}" | docker login --username ${docker_user} --password-stdin ${operatorRegistry}

            # Create tags
            docker tag ${operatorRepo}:${VERSION} ${operatorRegistry}/${operatorRepo}:${VERSION}
            docker tag ${operatorRepo}:${VERSION} ${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}
            docker tag ${operatorRepo}:${VERSION} ${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}-${timeStamp}
            docker tag ${operatorRepo}:${VERSION} ${operatorRegistry}/${operatorRepo}:latest

            # Push images to internal registry
            docker push ${operatorRegistry}/${operatorRepo}:${VERSION}
            docker push ${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}
            docker push ${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}-${timeStamp}
            docker push ${operatorRegistry}/${operatorRepo}:latest
        """
    }
}

pipeline {
    agent {
        label {
            label 'cld-kubernetes'
        }
    }
    options {
        checkoutToSubdirectory '.'
        buildDiscarder logRotator(artifactDaysToKeepStr: '20', artifactNumToKeepStr: '', daysToKeepStr: '30', numToKeepStr: '')
        skipStagesAfterUnstable()
    }
    
    triggers {
        // Trigger nightly builds on the develop branch
        parameterizedCron( env.BRANCH_NAME == 'develop' ? '''00 05 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12; E2E_SCOPE=both
                 00 05 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-11; PUBLISH_IMAGE=false; E2E_SCOPE=both
                 00 07 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12; VERIFY_ISTIO_AMBIENT=true; E2E_SCOPE=both
                 30 05 * * * % E2E_RUNTIME=eks; E2E_SCOPE=cluster; VERIFY_ISTIO_AMBIENT=true''' : '')
    }

    environment {
        PATH = "/space/go/bin:${env.PATH}"
        MINIKUBE_HOME = "/space/minikube/"
        KUBECONFIG = "/space/.kube-config"
        GOPATH = "/space/go"
    }


    parameters {
        string(name: 'E2E_MARKLOGIC_IMAGE_VERSION', defaultValue: 'ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12', description: 'Docker image to use for tests.', trim: true)
        string(name: 'VERSION', defaultValue: '1.3.0', description: 'Version to tag the image with.', trim: true)
        choice(name: 'E2E_INSTALL_MODE', choices: ['fresh', 'upgrade'], description: 'Run the standard fresh-install e2e flow or the upgrade validation flow. Default is fresh.')
        choice(name: 'E2E_SCOPE', choices: ['namespace-only', 'cluster', 'both', 'dynamic-host', 'volume-resize'], description: 'Combined test selector: namespace-only runs Helm namespace suite, cluster runs full cluster suite, both runs cluster+namespace in parallel minikube shards, dynamic-host and volume-resize run focused cluster tests.')
        choice(name: 'E2E_RUNTIME', choices: ['minikube', 'eks'], description: 'Execution runtime for e2e tests. minikube runs profile-parallel shards; eks runs the EKS suite only.')
        booleanParam(name: 'PUBLISH_IMAGE', defaultValue: false, description: 'Publish image to internal registry')
        string(name: 'emailList', defaultValue: emailList, description: 'List of email for build notification', trim: true)
        booleanParam(name: 'VERIFY_ISTIO_AMBIENT', defaultValue: true, description: 'Run Istio ambient mode e2e tests (Istio ambient mode is installed alongside the regular Minikube/EKS cluster; no dedicated cluster is created).')
        string(name: 'EKS_MARKLOGIC_IMAGE_TAG', defaultValue: 'latest-12', description: 'MarkLogic image tag to pull from the EKS ECR registry when E2E_RUNTIME=eks. The full ECR URL is constructed at runtime from the AWS account ID resolved via STS.', trim: true)
    }

    stages {
        stage('Pre-Build-Check') {
            steps {
                preBuildCheck()
            }
        }

        stage('Run-tests') {
            steps {
                runTests()
            }
        }

        // -----------------------------------------------------------------------
        // E2E Tests — run minikube shards in one shell step using background jobs.
        // This avoids Jenkins branch scheduling behavior and makes parallelism
        // explicit at the process level: cluster-scoped shard and namespace-scoped
        // shard run concurrently, each with its own MINIKUBE_PROFILE, KUBECONFIG,
        // and MINIKUBE_HOME.
        // -----------------------------------------------------------------------
        stage('E2E Tests') {
            steps {
                script {
                    def runOnEks = params.E2E_RUNTIME == 'eks'
                    def runClusterScoped = params.E2E_SCOPE in ['cluster', 'both', 'dynamic-host', 'volume-resize']
                    def runNamespaceScoped = params.E2E_SCOPE in ['namespace-only', 'both']
                    def clusterScope = params.E2E_SCOPE in ['dynamic-host', 'volume-resize'] ? params.E2E_SCOPE : 'cluster'

                    if (!runClusterScoped && !runNamespaceScoped) {
                        echo "No e2e suites selected (E2E_SCOPE=${params.E2E_SCOPE}); skipping e2e tests."
                        return
                    }

                    if (params.E2E_INSTALL_MODE == 'upgrade') {
                        if (runOnEks) {
                            error "E2E_INSTALL_MODE='upgrade' is only supported on the Minikube path right now. Use E2E_RUNTIME='minikube'."
                        }
                        if (clusterScope != 'cluster') {
                            error "E2E_INSTALL_MODE='upgrade' supports E2E_SCOPE values: cluster, namespace-only, both."
                        }
                    }

                    if (runOnEks) {
                        if (!runClusterScoped) {
                            error "E2E_RUNTIME='eks' requires E2E_SCOPE to include cluster tests."
                        }
                        if (runNamespaceScoped) {
                            error "E2E_RUNTIME='eks' does not support namespace-scoped tests. Set E2E_SCOPE='cluster'."
                        }
                        if (clusterScope != 'cluster') {
                            error "E2E_RUNTIME='eks' does not support focused plans '${params.E2E_SCOPE}'. Use E2E_SCOPE='cluster'."
                        }

                        def runIstioOnEKS = params.E2E_INSTALL_MODE == 'fresh' && params.VERIFY_ISTIO_AMBIENT
                        try {
                            lock(resource: 'jenkinsKubeNinjasEksCluster', inversePrecedence: true) {
                                timeout(time: 3, unit: 'HOURS') {
                                    stage('Setup') {
                                        if (runIstioOnEKS) { runEKSIstioSetup() }
                                        else               { runEKSSetup() }
                                    }
                                    stage('Run e2e Tests') { runEKSE2eTests() }
                                    stage('Run Istio e2e Tests') {
                                        if (runIstioOnEKS) { runEKSIstioE2eTests() }
                                        else { echo "Istio tests skipped (E2E_INSTALL_MODE=${params.E2E_INSTALL_MODE}, VERIFY_ISTIO_AMBIENT=${params.VERIFY_ISTIO_AMBIENT})" }
                                    }
                                }
                            }
                        } finally {
                            stage('Cleanup') {
                                catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE') {
                                    runEKSCleanup()
                                }
                            }
                        }
                        return
                    }

                    def clusterMinikubeProfile = 'e2e-cluster'
                    def namespaceMinikubeProfile = 'e2e-namespace'
                    def runIstio = params.E2E_INSTALL_MODE == 'fresh' && runClusterScoped && clusterScope == 'cluster' && params.VERIFY_ISTIO_AMBIENT
                    def clusterTestCommand = params.E2E_INSTALL_MODE == 'upgrade'
                        ? "make e2e-test-upgrade-cluster IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${clusterMinikubeProfile}"
                        : "make e2e-test-${clusterScope} IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${clusterMinikubeProfile}"
                    def namespaceTestCommand = params.E2E_INSTALL_MODE == 'upgrade'
                        ? "make e2e-test-upgrade-helm-namespace IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${namespaceMinikubeProfile}"
                        : "make e2e-test-helm-namespace IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${namespaceMinikubeProfile}"

                    sh """
                        set -euo pipefail

                        run_cluster_shard() {
                            export MINIKUBE_PROFILE='${clusterMinikubeProfile}'
                            export KUBECONFIG='/space/.kube-config-cluster'
                            export MINIKUBE_HOME='/space/minikube-cluster/'

                            echo '=====Starting cluster-scoped shard====='
                            make e2e-setup-minikube IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${clusterMinikubeProfile} MINIKUBE_REUSE=true
                            ${clusterTestCommand}
                            if [ '${runIstio}' = 'true' ]; then
                                make e2e-test-istio IMG=${operatorRepo}:${VERSION} E2E_ISTIO_AMBIENT=true MINIKUBE_PROFILE=${clusterMinikubeProfile}
                            else
                                echo '=====Istio tests skipped for cluster shard====='
                            fi
                            make e2e-cleanup-minikube MINIKUBE_PROFILE=${clusterMinikubeProfile} MINIKUBE_REUSE=true
                            echo '=====Cluster-scoped shard complete====='
                        }

                        run_namespace_shard() {
                            export MINIKUBE_PROFILE='${namespaceMinikubeProfile}'
                            export KUBECONFIG='/space/.kube-config-namespace'
                            export MINIKUBE_HOME='/space/minikube-namespace/'

                            echo '=====Starting namespace-scoped shard====='
                            make e2e-setup-minikube IMG=${operatorRepo}:${VERSION} MINIKUBE_PROFILE=${namespaceMinikubeProfile} MINIKUBE_REUSE=true
                            ${namespaceTestCommand}
                            make e2e-cleanup-minikube MINIKUBE_PROFILE=${namespaceMinikubeProfile} MINIKUBE_REUSE=true
                            echo '=====Namespace-scoped shard complete====='
                        }

                        cluster_pid=''
                        namespace_pid=''

                        if [ '${runClusterScoped}' = 'true' ]; then
                            (run_cluster_shard) &
                            cluster_pid=\$!
                        fi

                        if [ '${runNamespaceScoped}' = 'true' ]; then
                            (run_namespace_shard) &
                            namespace_pid=\$!
                        fi

                        rc=0

                        if [ -n "\$cluster_pid" ]; then
                            if ! wait "\$cluster_pid"; then
                                rc=1
                            fi
                        fi

                        if [ -n "\$namespace_pid" ]; then
                            if ! wait "\$namespace_pid"; then
                                rc=1
                            fi
                        fi

                        exit "\$rc"
                    """
                }
            }
        }

        // Publish image to internal registries (conditional)
        stage('Publish Image') {
            when {
                    anyOf {
                        branch 'develop'
                        expression { return params.PUBLISH_IMAGE }
                    }
            }
            steps {
                publishToInternalRegistry()
            }
        }

        stage('Run-BlackDuck-Scan') {
            when {
                branch pattern: '^(develop|main|release.*)$', comparator: 'REGEXP'
            }
            steps {
                runBlackDuckScan()
            }
        }
        
    }

    post {
        always {
            publishTestResults()
        }
        success {
            resultNotification('✅ Success')
        }
        failure {
            resultNotification('❌ Failure')
        }
        unstable {
            resultNotification('⚠️ Unstable')
        }
        aborted {
            resultNotification('🚫 Aborted')
        }
    }
}