// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

// Parameters are passed as REST /v1/eval external variables, never interpolated
// into executable XQuery. Control queries run in Schemas so restoring Documents
// cannot take the query's own database offline.
const forestsQuery = `object-node {
 "forests": array-node { for $id in xdmp:database-forests(xdmp:database("Documents"))
 return object-node {"id": string($id), "name": xdmp:forest-name($id)} }
}`

const writeQuery = `declare variable $run as xs:string external;
declare variable $value as xs:string external;
xdmp:document-insert("/integration/backup.json", object-node {"run": $run, "value": $value}),
object-node {"written": true()}`

const readQuery = `fn:doc("/integration/backup.json")`

const directoryQuery = `declare variable $directory as xs:string external;
xdmp:filesystem-directory-create($directory,
 <options xmlns="xdmp:filesystem-directory-create"><create-parents>true</create-parents></options>),
object-node {"created": true()}`

const backupQuery = `declare variable $directory as xs:string external;
object-node {"job": string(xdmp:database-backup(xdmp:database-forests(xdmp:database("Documents")), $directory))}`

const restoreQuery = `declare variable $directory as xs:string external;
object-node {"job": string(xdmp:database-restore(xdmp:database-forests(xdmp:database("Documents")), $directory))}`

const backupStatusQuery = `declare variable $job as xs:string external;
let $s := xdmp:database-backup-status(xs:unsignedLong($job))
return object-node {"status": string($s/*:status), "forests": array-node {
 for $f in $s/*:forest return object-node {
 "id": string($f/*:forest-id), "name": string($f/*:forest-name),
 "status": string($f/*:status), "backupPath": string($f/*:backup-path)
 }}}`

const restoreStatusQuery = `declare variable $job as xs:string external;
let $s := xdmp:database-restore-status(xs:unsignedLong($job))
return object-node {"status": string($s/*:status), "forests": array-node {
 for $f in $s/*:forest return object-node {
 "id": string($f/*:forest-id), "name": string($f/*:forest-name), "status": string($f/*:status)
 }}}`
