package store

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TableDefinition describes the table for local development and tests. The
// real one is created by infra/vidwatch.yaml — keep the two in sync.
func TableDefinition(name string) *dynamodb.CreateTableInput {
	attr := func(n string) types.AttributeDefinition {
		return types.AttributeDefinition{AttributeName: aws.String(n), AttributeType: types.ScalarAttributeTypeS}
	}
	keys := func(hash, rng string) []types.KeySchemaElement {
		return []types.KeySchemaElement{
			{AttributeName: aws.String(hash), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(rng), KeyType: types.KeyTypeRange},
		}
	}
	all := &types.Projection{ProjectionType: types.ProjectionTypeAll}
	return &dynamodb.CreateTableInput{
		TableName:   aws.String(name),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			attr("PK"), attr("SK"), attr("LSI1SK"), attr("GSI1PK"), attr("GSI1SK"), attr("GSI2PK"), attr("GSI2SK"),
		},
		KeySchema: keys("PK", "SK"),
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{
			{IndexName: aws.String("LSI1"), KeySchema: keys("PK", "LSI1SK"), Projection: all},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{IndexName: aws.String("GSI1"), KeySchema: keys("GSI1PK", "GSI1SK"), Projection: all},
			{IndexName: aws.String("GSI2"), KeySchema: keys("GSI2PK", "GSI2SK"), Projection: all},
		},
	}
}
