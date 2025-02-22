// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package serverccl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl/licenseccl"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilities"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/systemconfigwatcher/systemconfigwatchertest"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlinstance/instancestorage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestSQLServer starts up a semi-dedicated SQL server and runs some smoke test
// queries.
func TestSQLServer(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	ts, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		// This test is specific to secondary tenants; no need to run it
		// using the system tenant.
		DefaultTestTenant: base.TestTenantAlwaysEnabled,
	})
	defer ts.Stopper().Stop(ctx)

	r := sqlutils.MakeSQLRunner(db)
	r.QueryStr(t, `SELECT 1`)
	r.Exec(t, `CREATE DATABASE foo`)
	r.Exec(t, `CREATE TABLE foo.kv (k STRING PRIMARY KEY, v STRING)`)
	r.Exec(t, `INSERT INTO foo.kv VALUES('foo', 'bar')`)
	// Cause an index backfill operation.
	r.Exec(t, `CREATE INDEX ON foo.kv (v)`)
	t.Log(sqlutils.MatrixToStr(r.QueryStr(t, `SET distsql=off; SELECT * FROM foo.kv`)))
	t.Log(sqlutils.MatrixToStr(r.QueryStr(t, `SET distsql=auto; SELECT * FROM foo.kv`)))
}

func TestTenantCannotSetClusterSetting(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	tc := serverutils.StartNewTestCluster(t, 1, base.TestClusterArgs{ServerArgs: base.TestServerArgs{
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	}})
	defer tc.Stopper().Stop(ctx)

	// StartTenant with the default permissions to
	_, db := serverutils.StartTenant(t, tc.Server(0), base.TestTenantArgs{TenantID: serverutils.TestTenantID()})
	defer db.Close()
	_, err := db.Exec(`SET CLUSTER SETTING sql.defaults.vectorize=off`)
	require.NoError(t, err)
	_, err = db.Exec(`SET CLUSTER SETTING kv.snapshot_rebalance.max_rate = '2MiB';`)
	var pqErr *pq.Error
	ok := errors.As(err, &pqErr)
	require.True(t, ok, "expected err to be a *pq.Error but is of type %T. error is: %v", err)
	if !strings.Contains(pqErr.Message, "unknown cluster setting") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTenantCanUseEnterpriseFeatures verifies that tenants can get a license
// from the env variable.
func TestTenantCanUseEnterpriseFeatures(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	license, _ := (&licenseccl.License{
		Type: licenseccl.License_Enterprise,
	}).Encode()

	defer ccl.TestingDisableEnterprise()()
	defer envutil.TestSetEnv(t, "COCKROACH_TENANT_LICENSE", license)()

	s, _, _ := serverutils.StartServer(t, base.TestServerArgs{
		// Note: we can't use `TestTenantAlwaysEnabled` here because
		// (currently) that requires the enterprise license to be set at
		// the storage layer, which we just disabled above (because we
		// want to check the effects of the env var instead).
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	})
	defer s.Stopper().Stop(context.Background())

	_, db := serverutils.StartTenant(t, s, base.TestTenantArgs{TenantID: serverutils.TestTenantID()})
	defer db.Close()

	_, err := db.Exec(`BACKUP INTO 'userfile:///backup'`)
	require.NoError(t, err)
	_, err = db.Exec(`BACKUP INTO LATEST IN 'userfile:///backup'`)
	require.NoError(t, err)
}

func TestTenantUnauthenticatedAccess(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	s, _, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	})
	defer s.Stopper().Stop(ctx)

	_, err := s.StartTenant(ctx,
		base.TestTenantArgs{
			TenantID: roachpb.MustMakeTenantID(security.EmbeddedTenantIDs()[0]),
			TestingKnobs: base.TestingKnobs{
				TenantTestingKnobs: &sql.TenantTestingKnobs{
					// Configure the SQL server to access the wrong tenant keyspace.
					TenantIDCodecOverride: roachpb.MustMakeTenantID(security.EmbeddedTenantIDs()[1]),
				},
			},
		})
	require.Error(t, err)
	require.Regexp(t, `requested key .* not fully contained in tenant keyspace /Tenant/1{0-1}.*Unauthenticated`, err)
}

// TestTenantHTTP verifies that SQL tenant servers expose metrics and debugging endpoints.
func TestTenantHTTP(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	s, _, _ := serverutils.StartServer(t, base.TestServerArgs{
		// This test is specific to secondary tenants; no need to run it
		// using the system tenant.
		DefaultTestTenant: base.TestTenantAlwaysEnabled,
	})
	defer s.Stopper().Stop(ctx)

	ts := s.ApplicationLayer()

	t.Run("prometheus", func(t *testing.T) {
		httpClient, err := ts.GetUnauthenticatedHTTPClient()
		require.NoError(t, err)
		defer httpClient.CloseIdleConnections()
		resp, err := httpClient.Get(ts.AdminURL().WithPath("/_status/vars").String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "sql_ddl_started_count_internal")
	})
	t.Run("pprof", func(t *testing.T) {
		httpClient, err := ts.GetAdminHTTPClient()
		require.NoError(t, err)
		defer httpClient.CloseIdleConnections()
		u := ts.AdminURL().WithPath("/debug/pprof/goroutine")
		q := u.Query()
		q.Set("debug", "2")
		u.RawQuery = q.Encode()
		resp, err := httpClient.Get(u.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "goroutine")
	})
}

// TestTenantProcessDebugging verifies that in-process SQL tenant servers gate
// process debugging behind capabilities.
func TestTenantProcessDebugging(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	})
	defer s.Stopper().Stop(ctx)

	tenant, _, err := s.StartSharedProcessTenant(ctx,
		base.TestSharedProcessTenantArgs{
			TenantID:   serverutils.TestTenantID(),
			TenantName: "processdebug",
		})
	require.NoError(t, err)
	defer tenant.Stopper().Stop(ctx)

	t.Run("system tenant pprof", func(t *testing.T) {
		httpClient, err := s.GetAdminHTTPClient()
		require.NoError(t, err)
		defer httpClient.CloseIdleConnections()

		url := s.AdminURL().URL
		url.Path = url.Path + "/debug/pprof/goroutine"
		q := url.Query()
		q.Add("debug", "2")
		url.RawQuery = q.Encode()

		resp, err := httpClient.Get(url.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(body), "goroutine")
	})

	t.Run("pprof", func(t *testing.T) {
		httpClient, err := tenant.GetAdminHTTPClient()
		require.NoError(t, err)
		defer httpClient.CloseIdleConnections()

		url := tenant.AdminURL().URL
		url.Path = url.Path + "/debug/pprof/"
		q := url.Query()
		q.Add("debug", "2")
		url.RawQuery = q.Encode()

		resp, err := httpClient.Get(url.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Contains(t, string(body), "tenant does not have capability to debug the running process")

		_, err = db.Exec(`ALTER TENANT processdebug GRANT CAPABILITY can_debug_process=true`)
		require.NoError(t, err)

		serverutils.WaitForTenantCapabilities(t, s, serverutils.TestTenantID(), map[tenantcapabilities.ID]string{
			tenantcapabilities.CanDebugProcess: "true",
		}, "")

		resp, err = httpClient.Get(url.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(body), "goroutine")

		_, err = db.Exec(`ALTER TENANT processdebug REVOKE CAPABILITY can_debug_process`)
		require.NoError(t, err)

		serverutils.WaitForTenantCapabilities(t, s, serverutils.TestTenantID(), map[tenantcapabilities.ID]string{
			tenantcapabilities.CanDebugProcess: "false",
		}, "")
	})

	t.Run("vmodule", func(t *testing.T) {
		httpClient, err := tenant.GetAdminHTTPClient()
		require.NoError(t, err)
		defer httpClient.CloseIdleConnections()

		url := tenant.AdminURL().URL
		url.Path = url.Path + "/debug/vmodule"
		q := url.Query()
		q.Add("duration", "-1s")
		q.Add("vmodule", "exec_log=3")
		url.RawQuery = q.Encode()

		resp, err := httpClient.Get(url.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Contains(t, string(body), "tenant does not have capability to debug the running process")

		_, err = db.Exec(`ALTER TENANT processdebug GRANT CAPABILITY can_debug_process=true`)
		require.NoError(t, err)

		serverutils.WaitForTenantCapabilities(t, s, serverutils.TestTenantID(), map[tenantcapabilities.ID]string{
			tenantcapabilities.CanDebugProcess: "true",
		}, "")

		resp, err = httpClient.Get(url.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(body), "previous vmodule configuration: \nnew vmodule configuration: exec_log=3\n")

		_, err = db.Exec(`ALTER TENANT processdebug REVOKE CAPABILITY can_debug_process`)
		require.NoError(t, err)

		serverutils.WaitForTenantCapabilities(t, s, serverutils.TestTenantID(), map[tenantcapabilities.ID]string{
			tenantcapabilities.CanDebugProcess: "false",
		}, "")
	})
}

func TestNonExistentTenant(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	s, _, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	})
	defer s.Stopper().Stop(ctx)

	_, err := s.StartTenant(ctx,
		base.TestTenantArgs{
			TenantID:            serverutils.TestTenantID(),
			DisableCreateTenant: true,
			SkipTenantCheck:     true,

			SkipWaitForTenantCache: true,

			TestingKnobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					ShutdownTenantConnectorEarlyIfNoRecordPresent: true,
				},
			},
		})
	require.True(t, errors.Is(err, &kvpb.MissingRecordError{}))
}

// TestTenantRowIDs confirms `unique_rowid()` works as expected in a
// multi-tenant setup.
func TestTenantRowIDs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TestTenantAlwaysEnabled,
	})
	defer s.Stopper().Stop(ctx)
	const numRows = 10
	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlDB.Exec(t, `CREATE TABLE foo(key INT PRIMARY KEY DEFAULT unique_rowid(), val INT)`)
	sqlDB.Exec(t, fmt.Sprintf("INSERT INTO foo (val) SELECT * FROM generate_series(1, %d)", numRows))

	// Verify that the rows are inserted successfully and that the row ids
	// are based on the SQL instance ID.
	rows := sqlDB.Query(t, "SELECT key FROM foo")
	defer rows.Close()
	rowCount := 0
	instanceID := int(s.ApplicationLayer().SQLInstanceID())
	for rows.Next() {
		var key int
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		require.Equal(t, instanceID, key&instanceID)
		rowCount++
	}
	require.Equal(t, numRows, rowCount)
}

// TestTenantInstanceIDReclaimLoop confirms that the sql_instances reclaim loop
// has been started.
func TestTenantInstanceIDReclaimLoop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	clusterSettings := cluster.MakeTestingClusterSettings()
	s, _, _ := serverutils.StartServer(t, base.TestServerArgs{
		Settings:          clusterSettings,
		DefaultTestTenant: base.TestControlsTenantsExplicitly,
	})
	defer s.Stopper().Stop(ctx)

	instancestorage.ReclaimLoopInterval.Override(ctx, &clusterSettings.SV, 250*time.Millisecond)
	instancestorage.PreallocatedCount.Override(ctx, &clusterSettings.SV, 5)

	_, db := serverutils.StartTenant(
		t, s, base.TestTenantArgs{TenantID: serverutils.TestTenantID(), Settings: clusterSettings},
	)
	defer db.Close()
	sqlDB := sqlutils.MakeSQLRunner(db)

	var rowCount int64
	testutils.SucceedsSoon(t, func() error {
		sqlDB.QueryRow(t, `SELECT count(*) FROM system.sql_instances WHERE addr IS NULL`).Scan(&rowCount)
		// We set PreallocatedCount to 5. When the tenant gets started, it drops
		// to 4. Eventually this will be 5 if the reclaim loop runs.
		if rowCount == 5 {
			return nil
		}
		return fmt.Errorf("waiting for preallocated rows")
	})
}

func TestSystemConfigWatcherCache(t *testing.T) {
	defer leaktest.AfterTest(t)()
	systemconfigwatchertest.TestSystemConfigWatcher(t, false /* skipSecondary */)
}
