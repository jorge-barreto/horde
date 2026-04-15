package store

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestGSINames_MatchConstants(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		GSIByRepo: true, GSIByTicket: true, GSIByStatus: true, GSIByInstance: true,
	}
	got := map[string]bool{}
	for _, gsi := range GlobalSecondaryIndexes {
		got[*gsi.IndexName] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("expected GSI name %q not found in GlobalSecondaryIndexes", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected GSI name: got %q", name)
		}
	}
}

func TestAttributeDefinitions_CoverAllKeyAttributes(t *testing.T) {
	t.Parallel()
	keyAttrs := map[string]bool{}
	for _, el := range TableKeySchema {
		keyAttrs[*el.AttributeName] = true
	}
	for _, gsi := range GlobalSecondaryIndexes {
		for _, el := range gsi.KeySchema {
			keyAttrs[*el.AttributeName] = true
		}
	}
	defined := map[string]bool{}
	for _, ad := range AttributeDefinitions {
		defined[*ad.AttributeName] = true
	}
	for attr := range keyAttrs {
		if !defined[attr] {
			t.Errorf("key attribute %q not in AttributeDefinitions", attr)
		}
	}
}

func TestAttributeDefinitions_NoExtraAttributes(t *testing.T) {
	t.Parallel()
	keyAttrs := map[string]bool{}
	for _, el := range TableKeySchema {
		keyAttrs[*el.AttributeName] = true
	}
	for _, gsi := range GlobalSecondaryIndexes {
		for _, el := range gsi.KeySchema {
			keyAttrs[*el.AttributeName] = true
		}
	}
	for _, ad := range AttributeDefinitions {
		if !keyAttrs[*ad.AttributeName] {
			t.Errorf("attribute %q in AttributeDefinitions but not in any key schema", *ad.AttributeName)
		}
	}
}

func TestGSIByInstance_NoSortKey(t *testing.T) {
	t.Parallel()
	var byInstance *types.GlobalSecondaryIndex
	for i := range GlobalSecondaryIndexes {
		if *GlobalSecondaryIndexes[i].IndexName == GSIByInstance {
			byInstance = &GlobalSecondaryIndexes[i]
			break
		}
	}
	if byInstance == nil {
		t.Fatalf("GSI %q not found in GlobalSecondaryIndexes", GSIByInstance)
	}
	if got := len(byInstance.KeySchema); got != 1 {
		t.Errorf("GSI %q key schema: got %d elements, want 1", GSIByInstance, got)
	}
}

func TestGSIByInstance_ProjectionAll(t *testing.T) {
	t.Parallel()
	var byInstance *types.GlobalSecondaryIndex
	for i := range GlobalSecondaryIndexes {
		if *GlobalSecondaryIndexes[i].IndexName == GSIByInstance {
			byInstance = &GlobalSecondaryIndexes[i]
			break
		}
	}
	if byInstance == nil {
		t.Fatalf("GSI %q not found in GlobalSecondaryIndexes", GSIByInstance)
	}
	if got := byInstance.Projection.ProjectionType; got != types.ProjectionTypeAll {
		t.Errorf("GSI %q projection: got %v, want %v", GSIByInstance, got, types.ProjectionTypeAll)
	}
}

func TestTableKeySchema_SingleHashKey(t *testing.T) {
	t.Parallel()
	if got := len(TableKeySchema); got != 1 {
		t.Errorf("TableKeySchema length: got %d, want 1", got)
	}
	if got := *TableKeySchema[0].AttributeName; got != AttrID {
		t.Errorf("TableKeySchema[0].AttributeName: got %q, want %q", got, AttrID)
	}
	if got := TableKeySchema[0].KeyType; got != types.KeyTypeHash {
		t.Errorf("TableKeySchema[0].KeyType: got %v, want %v", got, types.KeyTypeHash)
	}
}

func TestGlobalSecondaryIndexes_Count(t *testing.T) {
	t.Parallel()
	if got := len(GlobalSecondaryIndexes); got != 4 {
		t.Errorf("GSI count: got %d, want 4", got)
	}
}
