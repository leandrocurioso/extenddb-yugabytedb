package compat

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
)

func tableArn(t *T, name string) string {
	out, err := t.DB.DescribeTable(t, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
	t.NoErr(err, "DescribeTable")
	return aws.ToString(out.Table.TableArn)
}

func adminCases() []Case {
	return []Case{
		{ID: "tags/tag-list-untag", Group: "tags", Ops: []string{"TagResource", "ListTagsOfResource", "UntagResource"}, run: func(t *T) {
			arn := tableArn(t, t.AdminTable())
			tags := func() map[string]string {
				got := map[string]string{}
				var next *string
				for {
					out, err := t.DB.ListTagsOfResource(t, &dynamodb.ListTagsOfResourceInput{ResourceArn: aws.String(arn), NextToken: next})
					t.NoErr(err, "ListTagsOfResource")
					for _, tag := range out.Tags {
						got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
					}
					if next = out.NextToken; next == nil {
						return got
					}
				}
			}
			_, err := t.DB.TagResource(t, &dynamodb.TagResourceInput{ResourceArn: aws.String(arn), Tags: []types.Tag{
				{Key: aws.String("env"), Value: aws.String("test")}, {Key: aws.String("team"), Value: aws.String("cells")},
			}})
			t.NoErr(err, "TagResource")
			got := tags()
			t.Check(got["env"] == "test" && got["team"] == "cells", "tags = %v", got)

			_, err = t.DB.TagResource(t, &dynamodb.TagResourceInput{ResourceArn: aws.String(arn), Tags: []types.Tag{
				{Key: aws.String("env"), Value: aws.String("prod")},
			}})
			t.NoErr(err, "TagResource overwrite")
			_, err = t.DB.UntagResource(t, &dynamodb.UntagResourceInput{ResourceArn: aws.String(arn), TagKeys: []string{"team"}})
			t.NoErr(err, "UntagResource")
			got = tags()
			_, hasTeam := got["team"]
			t.Check(got["env"] == "prod" && !hasTeam, "tags after overwrite and untag = %v", got)

			_, err = t.DB.ListTagsOfResource(t, &dynamodb.ListTagsOfResourceInput{ResourceArn: aws.String(arn + "-missing")})
			t.ErrCode(err, "ResourceNotFoundException", "ListTagsOfResource on a missing table")
		}},

		{ID: "ttl/update-describe", Group: "ttl", Ops: []string{"UpdateTimeToLive", "DescribeTimeToLive"}, run: func(t *T) {
			name := t.TableName("ttlcfg")
			t.CreateTable(simpleTable(name))
			defer t.DropTable(name)

			desc, err := t.DB.DescribeTimeToLive(t, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(name)})
			t.NoErr(err, "DescribeTimeToLive")
			t.Check(desc.TimeToLiveDescription != nil && desc.TimeToLiveDescription.TimeToLiveStatus == types.TimeToLiveStatusDisabled,
				"TTL on a new table = %+v", desc.TimeToLiveDescription)

			set := func(enabled bool) error {
				_, err := t.DB.UpdateTimeToLive(t, &dynamodb.UpdateTimeToLiveInput{
					TableName:               aws.String(name),
					TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String("expires_at"), Enabled: aws.Bool(enabled)},
				})
				return err
			}
			t.NoErr(set(true), "UpdateTimeToLive enable")
			t.Eventually(30*time.Second, "TTL ENABLED", func() bool {
				desc, err := t.DB.DescribeTimeToLive(t, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(name)})
				t.NoErr(err, "DescribeTimeToLive")
				d := desc.TimeToLiveDescription
				return d.TimeToLiveStatus == types.TimeToLiveStatusEnabled && aws.ToString(d.AttributeName) == "expires_at"
			})
			t.ErrCode(set(true), "ValidationException", "enabling TTL that is already enabled")
		}},

		{ID: "ttl/expiry", Group: "ttl", Ops: []string{"UpdateTimeToLive"}, run: func(t *T) {
			name := t.TableName("ttlexp")
			t.CreateTable(simpleTable(name))
			defer t.DropTable(name)
			_, err := t.DB.UpdateTimeToLive(t, &dynamodb.UpdateTimeToLiveInput{
				TableName:               aws.String(name),
				TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String("expires_at"), Enabled: aws.Bool(true)},
			})
			t.NoErr(err, "UpdateTimeToLive")

			nowSec := time.Now().Unix()
			t.Put(name, item{"pk": S("expired"), "expires_at": N(nowSec - 60)})
			t.Put(name, item{"pk": S("future"), "expires_at": N(nowSec + 3600)})
			t.Put(name, item{"pk": S("no-ttl")})
			t.Put(name, item{"pk": S("not-a-number"), "expires_at": S("soon")})
			// DynamoDB only promises deletion within days; ExtendDB sweeps within minutes.
			t.Eventually(4*time.Minute, "the expired item is deleted", func() bool {
				return t.Get(name, item{"pk": S("expired")}) == nil
			})
			for _, pk := range []string{"future", "no-ttl", "not-a-number"} {
				t.Check(t.Get(name, item{"pk": S(pk)}) != nil, "TTL deleted the %q item", pk)
			}
		}},

		{ID: "backup/create-restore-delete", Group: "backup",
			Ops: []string{"CreateBackup", "DescribeBackup", "ListBackups", "RestoreTableFromBackup", "DeleteBackup"}, run: func(t *T) {
				source, target := t.AdminTable(), t.TableName("restored")
				t.Put(source, item{"pk": S(t.PK("row")), "v": N(1)})

				backupName := fmt.Sprintf("b%d", time.Now().UnixNano())
				created, err := t.DB.CreateBackup(t, &dynamodb.CreateBackupInput{TableName: aws.String(source), BackupName: aws.String(backupName)})
				t.NoErr(err, "CreateBackup")
				arn := created.BackupDetails.BackupArn
				t.Check(aws.ToString(arn) != "" && aws.ToString(created.BackupDetails.BackupName) == backupName, "BackupDetails = %+v", created.BackupDetails)
				defer t.DB.DeleteBackup(t, &dynamodb.DeleteBackupInput{BackupArn: arn})

				t.Eventually(time.Minute, "backup AVAILABLE", func() bool {
					out, err := t.DB.DescribeBackup(t, &dynamodb.DescribeBackupInput{BackupArn: arn})
					t.NoErr(err, "DescribeBackup")
					t.Check(out.BackupDescription.SourceTableDetails != nil &&
						aws.ToString(out.BackupDescription.SourceTableDetails.TableName) == source, "backup source table = %+v", out.BackupDescription.SourceTableDetails)
					return out.BackupDescription.BackupDetails.BackupStatus == types.BackupStatusAvailable
				})
				list, err := t.DB.ListBackups(t, &dynamodb.ListBackupsInput{TableName: aws.String(source)})
				t.NoErr(err, "ListBackups")
				found := false
				for _, b := range list.BackupSummaries {
					found = found || aws.ToString(b.BackupArn) == aws.ToString(arn)
				}
				t.Check(found, "ListBackups does not list the new backup")

				// The backup is a snapshot: later writes must not show up in the restore.
				t.Put(source, item{"pk": S(t.PK("row")), "v": N(2)})
				_, err = t.DB.RestoreTableFromBackup(t, &dynamodb.RestoreTableFromBackupInput{BackupArn: arn, TargetTableName: aws.String(target)})
				t.NoErr(err, "RestoreTableFromBackup")
				defer t.DropTable(target)
				t.WaitActive(target)
				t.Check(ddbx.Int(t.Get(target, item{"pk": S(t.PK("row"))})["v"]) == 1, "restored item = %v, want the value at backup time", t.Get(target, item{"pk": S(t.PK("row"))}))

				_, err = t.DB.DeleteBackup(t, &dynamodb.DeleteBackupInput{BackupArn: arn})
				t.NoErr(err, "DeleteBackup")
				_, err = t.DB.DescribeBackup(t, &dynamodb.DescribeBackupInput{BackupArn: arn})
				t.ErrCode(err, "BackupNotFoundException", "DescribeBackup after DeleteBackup")
			}},

		{ID: "backup/point-in-time", Group: "backup",
			Ops: []string{"DescribeContinuousBackups", "UpdateContinuousBackups", "RestoreTableToPointInTime"}, run: func(t *T) {
				source, target := t.TableName("pitrsrc"), t.TableName("pitrdst")
				t.CreateTable(simpleTable(source))
				defer t.DropTable(source)

				desc, err := t.DB.DescribeContinuousBackups(t, &dynamodb.DescribeContinuousBackupsInput{TableName: aws.String(source)})
				t.NoErr(err, "DescribeContinuousBackups")
				d := desc.ContinuousBackupsDescription
				t.Check(d != nil && d.PointInTimeRecoveryDescription != nil &&
					d.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus == types.PointInTimeRecoveryStatusDisabled,
					"point-in-time recovery on a new table = %+v", d)

				upd, err := t.DB.UpdateContinuousBackups(t, &dynamodb.UpdateContinuousBackupsInput{
					TableName:                        aws.String(source),
					PointInTimeRecoverySpecification: &types.PointInTimeRecoverySpecification{PointInTimeRecoveryEnabled: aws.Bool(true)},
				})
				t.NoErr(err, "UpdateContinuousBackups")
				t.Check(upd.ContinuousBackupsDescription.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus == types.PointInTimeRecoveryStatusEnabled,
					"point-in-time recovery not enabled: %+v", upd.ContinuousBackupsDescription.PointInTimeRecoveryDescription)

				t.Put(source, item{"pk": S("row"), "v": N(1)})
				_, err = t.DB.RestoreTableToPointInTime(t, &dynamodb.RestoreTableToPointInTimeInput{
					SourceTableName: aws.String(source), TargetTableName: aws.String(target), UseLatestRestorableTime: aws.Bool(true),
				})
				t.NoErr(err, "RestoreTableToPointInTime")
				defer t.DropTable(target)
				t.WaitActive(target)
				t.Check(ddbx.Int(t.Get(target, item{"pk": S("row")})["v"]) == 1, "restored item = %v", t.Get(target, item{"pk": S("row")}))
			}},

		{ID: "export/table-to-point-in-time", Group: "import-export", Ops: []string{"ExportTableToPointInTime"}, run: func(t *T) {
			source := t.AdminTable()
			t.Put(source, item{"pk": S(t.PK("row")), "v": N(1)})
			out, err := t.DB.ExportTableToPointInTime(t, &dynamodb.ExportTableToPointInTimeInput{
				TableArn: aws.String(tableArn(t, source)), S3Bucket: aws.String("local"),
				S3Prefix: aws.String(t.exchangePath(source, "export.json")), ExportFormat: types.ExportFormatDynamodbJson,
			})
			t.NoErr(err, "ExportTableToPointInTime")
			d := out.ExportDescription
			t.Check(d != nil && aws.ToString(d.ExportArn) != "", "no ExportDescription: %+v", d)
			t.Check(d.ExportStatus == types.ExportStatusCompleted || d.ExportStatus == types.ExportStatusInProgress, "ExportStatus = %s", d.ExportStatus)
		}},

		{ID: "import/table", Group: "import-export", Ops: []string{"ImportTable"}, run: func(t *T) {
			source, target := t.AdminTable(), t.TableName("imported")
			t.Put(source, item{"pk": S(t.PK("row")), "v": N(1)})
			file := t.exchangePath(source, "import-source.json")
			_, err := t.DB.ExportTableToPointInTime(t, &dynamodb.ExportTableToPointInTimeInput{
				TableArn: aws.String(tableArn(t, source)), S3Bucket: aws.String("local"), S3Prefix: aws.String(file),
			})
			t.NoErr(err, "ExportTableToPointInTime (to produce the import source)")

			spec := simpleTable(target)
			out, err := t.DB.ImportTable(t, &dynamodb.ImportTableInput{
				InputFormat:    types.InputFormatDynamodbJson,
				S3BucketSource: &types.S3BucketSource{S3Bucket: aws.String("local"), S3KeyPrefix: aws.String(file)},
				TableCreationParameters: &types.TableCreationParameters{
					TableName: spec.TableName, AttributeDefinitions: spec.AttributeDefinitions,
					KeySchema: spec.KeySchema, BillingMode: spec.BillingMode,
				},
			})
			t.NoErr(err, "ImportTable")
			defer t.DropTable(target)
			t.Check(out.ImportTableDescription != nil, "no ImportTableDescription")
			t.WaitActive(target)
			t.Check(ddbx.Int(t.Get(target, item{"pk": S(t.PK("row"))})["v"]) == 1, "imported table misses the exported item")
		}},
	}
}

// exchangePath returns a fresh file path ExtendDB may write to: files live
// under <exchange dir>/<account id>, and an export never overwrites.
func (t *T) exchangePath(table, name string) string {
	arn := strings.Split(tableArn(t, table), ":")
	t.Check(len(arn) >= 6, "unexpected table ARN %v", arn)
	return path.Join(t.suite.exchangeDir, arn[4], fmt.Sprintf("%s-%d-%s", t.TableName(""), time.Now().UnixNano(), name))
}

// unsupported builds a case for an operation ExtendDB is not expected to
// implement: it must answer UnknownOperationException.
func unsupported(op string, documented bool, call func(t *T) error) Case {
	return Case{ID: "unsupported/" + op, Group: "unsupported", Ops: []string{op}, Unsupported: true, Documented: documented, run: func(t *T) {
		t.NoErr(call(t), op)
	}}
}

func unsupportedCases() []Case {
	const arn = "arn:aws:dynamodb:us-east-1:000000000000:table/none"
	name, stream := aws.String("none"), aws.String(arn+"/stream/x")
	return []Case{
		unsupported("ExecuteStatement", true, func(t *T) error {
			_, err := t.DB.ExecuteStatement(t, &dynamodb.ExecuteStatementInput{Statement: aws.String(`SELECT * FROM "none"`)})
			return err
		}),
		unsupported("BatchExecuteStatement", true, func(t *T) error {
			_, err := t.DB.BatchExecuteStatement(t, &dynamodb.BatchExecuteStatementInput{
				Statements: []types.BatchStatementRequest{{Statement: aws.String(`SELECT * FROM "none"`)}}})
			return err
		}),
		unsupported("ExecuteTransaction", true, func(t *T) error {
			_, err := t.DB.ExecuteTransaction(t, &dynamodb.ExecuteTransactionInput{
				TransactStatements: []types.ParameterizedStatement{{Statement: aws.String(`SELECT * FROM "none"`)}}})
			return err
		}),
		unsupported("CreateGlobalTable", true, func(t *T) error {
			_, err := t.DB.CreateGlobalTable(t, &dynamodb.CreateGlobalTableInput{
				GlobalTableName: name, ReplicationGroup: []types.Replica{{RegionName: aws.String("us-east-1")}}})
			return err
		}),
		unsupported("DescribeGlobalTable", true, func(t *T) error {
			_, err := t.DB.DescribeGlobalTable(t, &dynamodb.DescribeGlobalTableInput{GlobalTableName: name})
			return err
		}),
		unsupported("ListGlobalTables", true, func(t *T) error {
			_, err := t.DB.ListGlobalTables(t, &dynamodb.ListGlobalTablesInput{})
			return err
		}),
		unsupported("UpdateGlobalTable", true, func(t *T) error {
			_, err := t.DB.UpdateGlobalTable(t, &dynamodb.UpdateGlobalTableInput{
				GlobalTableName: name, ReplicaUpdates: []types.ReplicaUpdate{{Create: &types.CreateReplicaAction{RegionName: aws.String("us-west-2")}}}})
			return err
		}),
		unsupported("DescribeGlobalTableSettings", true, func(t *T) error {
			_, err := t.DB.DescribeGlobalTableSettings(t, &dynamodb.DescribeGlobalTableSettingsInput{GlobalTableName: name})
			return err
		}),
		unsupported("UpdateGlobalTableSettings", true, func(t *T) error {
			_, err := t.DB.UpdateGlobalTableSettings(t, &dynamodb.UpdateGlobalTableSettingsInput{
				GlobalTableName: name, GlobalTableBillingMode: types.BillingModePayPerRequest})
			return err
		}),
		unsupported("DescribeContributorInsights", true, func(t *T) error {
			_, err := t.DB.DescribeContributorInsights(t, &dynamodb.DescribeContributorInsightsInput{TableName: name})
			return err
		}),
		unsupported("UpdateContributorInsights", true, func(t *T) error {
			_, err := t.DB.UpdateContributorInsights(t, &dynamodb.UpdateContributorInsightsInput{
				TableName: name, ContributorInsightsAction: types.ContributorInsightsActionEnable})
			return err
		}),
		unsupported("ListContributorInsights", false, func(t *T) error {
			_, err := t.DB.ListContributorInsights(t, &dynamodb.ListContributorInsightsInput{})
			return err
		}),
		unsupported("DescribeKinesisStreamingDestination", true, func(t *T) error {
			_, err := t.DB.DescribeKinesisStreamingDestination(t, &dynamodb.DescribeKinesisStreamingDestinationInput{TableName: name})
			return err
		}),
		unsupported("EnableKinesisStreamingDestination", true, func(t *T) error {
			_, err := t.DB.EnableKinesisStreamingDestination(t, &dynamodb.EnableKinesisStreamingDestinationInput{TableName: name, StreamArn: stream})
			return err
		}),
		unsupported("DisableKinesisStreamingDestination", true, func(t *T) error {
			_, err := t.DB.DisableKinesisStreamingDestination(t, &dynamodb.DisableKinesisStreamingDestinationInput{TableName: name, StreamArn: stream})
			return err
		}),
		unsupported("UpdateKinesisStreamingDestination", false, func(t *T) error {
			_, err := t.DB.UpdateKinesisStreamingDestination(t, &dynamodb.UpdateKinesisStreamingDestinationInput{TableName: name, StreamArn: stream})
			return err
		}),
		unsupported("DescribeTableReplicaAutoScaling", true, func(t *T) error {
			_, err := t.DB.DescribeTableReplicaAutoScaling(t, &dynamodb.DescribeTableReplicaAutoScalingInput{TableName: name})
			return err
		}),
		unsupported("UpdateTableReplicaAutoScaling", true, func(t *T) error {
			_, err := t.DB.UpdateTableReplicaAutoScaling(t, &dynamodb.UpdateTableReplicaAutoScalingInput{TableName: name})
			return err
		}),
		unsupported("DescribeExport", false, func(t *T) error {
			_, err := t.DB.DescribeExport(t, &dynamodb.DescribeExportInput{ExportArn: aws.String(arn + "/export/x")})
			return err
		}),
		unsupported("ListExports", false, func(t *T) error {
			_, err := t.DB.ListExports(t, &dynamodb.ListExportsInput{})
			return err
		}),
		unsupported("DescribeImport", false, func(t *T) error {
			_, err := t.DB.DescribeImport(t, &dynamodb.DescribeImportInput{ImportArn: aws.String(arn + "/import/x")})
			return err
		}),
		unsupported("ListImports", false, func(t *T) error {
			_, err := t.DB.ListImports(t, &dynamodb.ListImportsInput{})
			return err
		}),
		unsupported("GetResourcePolicy", true, func(t *T) error {
			_, err := t.DB.GetResourcePolicy(t, &dynamodb.GetResourcePolicyInput{ResourceArn: aws.String(arn)})
			return err
		}),
		unsupported("PutResourcePolicy", true, func(t *T) error {
			_, err := t.DB.PutResourcePolicy(t, &dynamodb.PutResourcePolicyInput{ResourceArn: aws.String(arn), Policy: aws.String("{}")})
			return err
		}),
		unsupported("DeleteResourcePolicy", true, func(t *T) error {
			_, err := t.DB.DeleteResourcePolicy(t, &dynamodb.DeleteResourcePolicyInput{ResourceArn: aws.String(arn)})
			return err
		}),
	}
}
