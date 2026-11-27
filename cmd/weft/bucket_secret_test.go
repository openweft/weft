package main

// bucket_secret_test.go pins the write-only contract of the S3 bucket
// secret: CreateBucket stores it, no RPC ever returns it.

import (
	"testing"

	weftv1 "github.com/openweft/weft-proto"
)

const bucketTestSecret = "s3cr3t-never-returned"

func TestBucketSecretIsNeverReturned(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()

	check := func(rpc string, bi *weftv1.BucketInfo) {
		t.Helper()
		if bi == nil {
			t.Fatalf("%s: nil BucketInfo", rpc)
		}
		if bi.SecretAccessKey != "" { //nolint:staticcheck // deprecated field must stay empty
			t.Errorf("%s returned the secret access key", rpc)
		}
		if bi.AccessKeyId != "AKIATEST" {
			t.Errorf("%s access_key_id = %q, want AKIATEST", rpc, bi.AccessKeyId)
		}
	}

	req := &weftv1.CreateBucketRequest{
		Project: "proj-1", Name: "data", Endpoint: "https://s3.example.com",
		Region: "us-east-1", AccessKeyId: "AKIATEST", SecretAccessKey: bucketTestSecret,
	}
	first, err := s.CreateBucket(ctx, req)
	if err != nil || !first.Created {
		t.Fatalf("seed bucket: %+v err=%v", first, err)
	}
	b := first.Bucket
	check("CreateBucket (new)", b)
	// The secret is stored: the write path works.
	if got, _ := adp.BucketByUUID(b.Uuid); got.SecretAccessKey != bucketTestSecret {
		t.Fatalf("secret not stored: %q", got.SecretAccessKey)
	}

	// CreateBucket on an existing name returns the existing row: it must
	// not leak the stored secret either.
	again, err := s.CreateBucket(ctx, req)
	if err != nil || again.Created {
		t.Fatalf("second CreateBucket: %+v err=%v", again, err)
	}
	check("CreateBucket (existing)", again.Bucket)

	g, err := s.GetBucket(ctx, &weftv1.GetBucketRequest{Uuid: b.Uuid})
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	check("GetBucket", g.Bucket)

	l, err := s.ListBuckets(ctx, &weftv1.ListBucketsRequest{Project: b.ProjectUuid})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(l.Buckets) != 1 {
		t.Fatalf("ListBuckets returned %d buckets, want 1", len(l.Buckets))
	}
	check("ListBuckets", l.Buckets[0])

	p, err := s.SetBucketPolicy(ctx, &weftv1.SetBucketPolicyRequest{Uuid: b.Uuid, Policy: "{}"})
	if err != nil {
		t.Fatalf("SetBucketPolicy: %v", err)
	}
	check("SetBucketPolicy", p.Bucket)
}
