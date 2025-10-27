// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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

void runBlackDuckScan() {
    // Trigger BlackDuck scan job with CONTAINER_IMAGES parameter when params.PUBLISH_IMAGE is true
    if (params.PUBLISH_IMAGE) {
        build job: 'securityscans/Blackduck/KubeNinjas/kubernetes-operator', wait: false, parameters: [ string(name: 'branch', value: "${env.BRANCH_NAME}"), string(name: 'CONTAINER_IMAGES', value: "${operatorRepo}:${VERSION}-${branchNameTag}") ]
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
                                                             00 05 * * * % E2E_MARKLOGIC_IMAGE_VERSION=ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-11; PUBLISH_IMAGE=false''' : '')
    }

    parameters {
        string(name: 'E2E_MARKLOGIC_IMAGE_VERSION', defaultValue: 'ml-docker-db-dev-tierpoint.bed-artifactory.bedford.progress.com/marklogic/marklogic-server-ubi-rootless:latest-12', description: 'Docker image to use for tests.', trim: true)
        string(name: 'VERSION', defaultValue: '1.1.0', description: 'Version to tag the image with.', trim: true)
        booleanParam(name: 'PUBLISH_IMAGE', defaultValue: false, description: 'Publish image to internal registry')
        string(name: 'emailList', defaultValue: emailList, description: 'List of email for build notification', trim: true)
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

        stage('Run-Minikube-Setup') {
            steps {
                runMinikubeSetup()
            }
        }

        stage('Run-e2e-Tests') {
            steps {
                runE2eTests()
            }
        }

        stage('Cleanup Environment') {
            steps {
                runMinikubeCleanup()
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