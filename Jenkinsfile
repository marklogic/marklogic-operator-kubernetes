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

void runMinikubeSetup() {
    sh """
        make e2e-setup-minikube IMG=${operatorRepo}:${VERSION}
    """
}

void runE2eTests() {
    sh """
        make e2e-test IMG=${operatorRepo}:${VERSION}
    """
}

void runMinikubeCleanup() {
    sh '''
        make e2e-cleanup-minikube
    '''
}

void runIstioMinikubeSetup() {
    sh """
        make e2e-setup-minikube-istio IMG=${operatorRepo}:${VERSION}
    """
}

void runIstioE2eTests() {
    sh """
        make e2e-test-istio IMG=${operatorRepo}:${VERSION} E2E_ISTIO_AMBIENT=true
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

void runBlackDuckScan() {
    // Trigger BlackDuck scan job with CONTAINER_IMAGES parameter when params.PUBLISH_IMAGE is true
    if (params.PUBLISH_IMAGE) {
        build job: 'securityscans/Blackduck/KubeNinjas/kubernetes-operator', wait: false, parameters: [ string(name: 'branch', value: "${env.BRANCH_NAME}"), string(name: 'CONTAINER_IMAGES', value: "${operatorRegistry}/${operatorRepo}:${VERSION}-${branchNameTag}-${timeStamp}") ]
    } else {
        build job: 'securityscans/Blackduck/KubeNinjas/kubernetes-operator', wait: false, parameters: [ string(name: 'branch', value: "${env.BRANCH_NAME}") ]
    }
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
        parameterizedCron( env.BRANCH_NAME == 'develop' ? '''00 05 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12
                                                             00 05 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-11; PUBLISH_IMAGE=false
                                                             00 07 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12; VERIFY_ISTIO_AMBIENT=true
                                                             30 05 * * * % TEST_ON_EKS=true; VERIFY_ISTIO_AMBIENT=true''' : '')
    }

    environment {
        PATH = "/space/go/bin:${env.PATH}"
        MINIKUBE_HOME = "/space/minikube/"
        KUBECONFIG = "/space/.kube-config"
        GOPATH = "/space/go"
    }


    parameters {
        string(name: 'E2E_MARKLOGIC_IMAGE_VERSION', defaultValue: 'ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12', description: 'Docker image to use for tests.', trim: true)
        string(name: 'VERSION', defaultValue: '1.2.0', description: 'Version to tag the image with.', trim: true)
        booleanParam(name: 'PUBLISH_IMAGE', defaultValue: false, description: 'Publish image to internal registry')
        string(name: 'emailList', defaultValue: emailList, description: 'List of email for build notification', trim: true)
        booleanParam(name: 'VERIFY_ISTIO_AMBIENT', defaultValue: true, description: 'Run Istio ambient mode e2e tests (requires fresh minikube cluster with Istio)')
        booleanParam(name: 'TEST_ON_EKS', defaultValue: false, description: 'Run e2e tests on the EKS cluster (jenkins-kube-ninjas) instead of Minikube. Requires KUBE_NINJAS_OPS_AWS_JENKINS credentials on this agent.')
        string(name: 'EKS_MARKLOGIC_IMAGE_TAG', defaultValue: 'latest-12', description: 'MarkLogic image tag to pull from the EKS ECR registry when TEST_ON_EKS=true. The full ECR URL is constructed at runtime from the AWS account ID resolved via STS.', trim: true)
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
        // E2E Tests — runs on Minikube (default) or the shared EKS cluster.
        // Minikube and EKS paths are unified into the same named stages.
        // The EKS cluster lock is acquired only for EKS builds, so unrelated
        // Minikube builds are never blocked. Cleanup is guaranteed via
        // try/finally even when earlier stages throw.
        // -----------------------------------------------------------------------
        stage('E2E Tests') {
            steps {
                script {
                    def doSetup    = { params.TEST_ON_EKS ? runEKSSetup()          : runMinikubeSetup() }
                    def doTests    = { params.TEST_ON_EKS ? runEKSE2eTests()        : runE2eTests() }
                    def doCleanup  = {
                        catchError(buildResult: 'SUCCESS', stageResult: 'FAILURE') {
                            if (params.TEST_ON_EKS) { runEKSCleanup() } else { runMinikubeCleanup() }
                        }
                    }
                    def doIstioSetup  = { params.TEST_ON_EKS ? runEKSIstioSetup()      : runIstioMinikubeSetup() }
                    def doIstioTests  = { params.TEST_ON_EKS ? runEKSIstioE2eTests()   : runIstioE2eTests() }

                    def testBody = {
                        try {
                            stage('Setup')         { doSetup() }
                            stage('Run e2e Tests') { doTests() }
                        } finally {
                            stage('Cleanup')       { doCleanup() }
                        }
                        // Istio stages are always declared so that Jenkins Stage View
                        // shows a consistent set of columns across all run types.
                        // When VERIFY_ISTIO_AMBIENT is false the stages are entered but
                        // immediately skipped, preserving their position in the view.
                        try {
                            stage('Istio Setup') {
                                if (params.VERIFY_ISTIO_AMBIENT) { doIstioSetup() }
                                else { echo 'Istio tests skipped (VERIFY_ISTIO_AMBIENT=false)' }
                            }
                            stage('Run Istio e2e Tests') {
                                if (params.VERIFY_ISTIO_AMBIENT) { doIstioTests() }
                                else { echo 'Istio tests skipped (VERIFY_ISTIO_AMBIENT=false)' }
                            }
                        } finally {
                            stage('Istio Cleanup') {
                                if (params.VERIFY_ISTIO_AMBIENT) { doCleanup() }
                                else { echo 'Istio tests skipped (VERIFY_ISTIO_AMBIENT=false)' }
                            }
                        }
                    }

                    if (params.TEST_ON_EKS) {
                        lock(resource: 'jenkinsKubeNinjasEksCluster', inversePrecedence: true) {
                            timeout(time: 3, unit: 'HOURS') {
                                testBody()
                            }
                        }
                    } else {
                        testBody()
                    }
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