// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics_test

import (
	"fmt"

	"xmysql-server/server/innodb/domain"
	"xmysql-server/server/innodb/kv"
	"xmysql-server/server/innodb/model"
	"xmysql-server/server/innodb/util/testkit"
	"xmysql-server/server/innodb/util/testleak"
	"xmysql-server/server/innodb/util/types"
)

var _ = Suite(&testStatsUpdateSuite{})

type testStatsUpdateSuite struct {
	store kv.Storage
	do    *domain.Domain
}

func (s *testStatsUpdateSuite) SetUpSuite(c *C) {
	testleak.BeforeTest()
	var err error
	s.store, s.do, err = newStoreWithBootstrap()
	c.Assert(err, IsNil)
}

func (s *testStatsUpdateSuite) TearDownSuite(c *C) {
	s.do.Close()
	s.store.Close()
	testleak.AfterTest(c)()
}

func (s *testStatsUpdateSuite) TestSingleSessionInsert(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t1 (c1 int, c2 int)")
	testKit.MustExec("create table t2 (c1 int, c2 int)")

	rowCount1 := 10
	rowCount2 := 20
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}
	for i := 0; i < rowCount2; i++ {
		testKit.MustExec("insert into t2 values(1, 2)")
	}

	is := s.do.InfoSchema()
	tbl1, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	tableInfo1 := tbl1.Meta()
	h := s.do.StatsHandle()

	h.HandleDDLEvent(<-h.DDLEventCh())
	h.HandleDDLEvent(<-h.DDLEventCh())

	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 := h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1))

	tbl2, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t2"))
	c.Assert(err, IsNil)
	tableInfo2 := tbl2.Meta()
	stats2 := h.GetTableStats(tableInfo2.ID)
	c.Assert(stats2.Count, Equals, int64(rowCount2))

	testKit.MustExec("analyze table t1")
	// Test update in a txn.
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1*2))

	// Test IncreaseFactor.
	count, err := stats1.ColumnEqualRowCount(testKit.Se.GetSessionVars().StmtCtx, types.NewIntDatum(1), tableInfo1.Columns[0])
	c.Assert(err, IsNil)
	c.Assert(count, Equals, float64(rowCount1*2))

	testKit.MustExec("begin")
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}
	testKit.MustExec("commit")
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1*3))

	testKit.MustExec("begin")
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("delete from t1 limit 1")
	}
	for i := 0; i < rowCount2; i++ {
		testKit.MustExec("update t2 set c2 = c1")
	}
	testKit.MustExec("commit")
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1*3))
	stats2 = h.GetTableStats(tableInfo2.ID)
	c.Assert(stats2.Count, Equals, int64(rowCount2))

	testKit.MustExec("begin")
	testKit.MustExec("delete from t1")
	testKit.MustExec("commit")
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(0))

	rs := testKit.MustQuery("select modify_count from mysql.stats_meta")
	rs.Check(testkit.Rows("40", "70"))
}

func (s *testStatsUpdateSuite) TestRollback(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (a int, b int)")
	testKit.MustExec("begin")
	testKit.MustExec("insert into t values (1,2)")
	testKit.MustExec("rollback")

	is := s.do.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tableInfo := tbl.Meta()
	h := s.do.StatsHandle()
	h.HandleDDLEvent(<-h.DDLEventCh())
	h.DumpStatsDeltaToKV()
	h.Update(is)

	stats := h.GetTableStats(tableInfo.ID)
	c.Assert(stats.Count, Equals, int64(0))
	c.Assert(stats.ModifyCount, Equals, int64(0))
}

func (s *testStatsUpdateSuite) TestMultiSession(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t1 (c1 int, c2 int)")

	rowCount1 := 10
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}

	testKit1 := testkit.NewTestKit(c, s.store)
	for i := 0; i < rowCount1; i++ {
		testKit1.MustExec("insert into test.t1 values(1, 2)")
	}
	testKit2 := testkit.NewTestKit(c, s.store)
	for i := 0; i < rowCount1; i++ {
		testKit2.MustExec("delete from test.t1 limit 1")
	}
	is := s.do.InfoSchema()
	tbl1, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	tableInfo1 := tbl1.Meta()
	h := s.do.StatsHandle()

	h.HandleDDLEvent(<-h.DDLEventCh())

	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 := h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1))

	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(1, 2)")
	}

	for i := 0; i < rowCount1; i++ {
		testKit1.MustExec("insert into test.t1 values(1, 2)")
	}

	for i := 0; i < rowCount1; i++ {
		testKit2.MustExec("delete from test.t1 limit 1")
	}

	testKit.Se.Close()
	testKit2.Se.Close()

	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1*2))
	// The session in testKit is already Closed, set it to nil will create a new session.
	testKit.Se = nil
	rs := testKit.MustQuery("select modify_count from mysql.stats_meta")
	rs.Check(testkit.Rows("60"))
}

func (s *testStatsUpdateSuite) TestTxnWithFailure(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t1 (c1 int primary key, c2 int)")

	is := s.do.InfoSchema()
	tbl1, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	tableInfo1 := tbl1.Meta()
	h := s.do.StatsHandle()

	h.HandleDDLEvent(<-h.DDLEventCh())

	rowCount1 := 10
	testKit.MustExec("begin")
	for i := 0; i < rowCount1; i++ {
		testKit.MustExec("insert into t1 values(?, 2)", i)
	}
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 := h.GetTableStats(tableInfo1.ID)
	// have not commit
	c.Assert(stats1.Count, Equals, int64(0))
	testKit.MustExec("commit")

	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1))

	_, err = testKit.Exec("insert into t1 values(0, 2)")
	c.Assert(err, NotNil)

	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1))

	testKit.MustExec("insert into t1 values(-1, 2)")
	h.DumpStatsDeltaToKV()
	h.Update(is)
	stats1 = h.GetTableStats(tableInfo1.ID)
	c.Assert(stats1.Count, Equals, int64(rowCount1+1))
}

func (s *testStatsUpdateSuite) TestAutoUpdate(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (a int)")

	do := s.do
	is := do.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tableInfo := tbl.Meta()
	h := do.StatsHandle()

	h.HandleDDLEvent(<-h.DDLEventCh())
	h.Update(is)
	stats := h.GetTableStats(tableInfo.ID)
	c.Assert(stats.Count, Equals, int64(0))

	_, err = testKit.Exec("insert into t values (1)")
	c.Assert(err, IsNil)
	h.DumpStatsDeltaToKV()
	h.Update(is)
	err = h.HandleAutoAnalyze(is)
	c.Assert(err, IsNil)
	h.Update(is)
	stats = h.GetTableStats(tableInfo.ID)
	c.Assert(stats.Count, Equals, int64(1))
	c.Assert(stats.ModifyCount, Equals, int64(0))

	_, err = testKit.Exec("create index idx on t(a)")
	c.Assert(err, IsNil)
	is = do.InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tableInfo = tbl.Meta()
	h.HandleAutoAnalyze(is)
	h.Update(is)
	stats = h.GetTableStats(tableInfo.ID)
	c.Assert(stats.Count, Equals, int64(1))
	c.Assert(stats.ModifyCount, Equals, int64(0))
	hg, ok := stats.Indices[tableInfo.Indices[0].ID]
	c.Assert(ok, IsTrue)
	c.Assert(hg.NDV, Equals, int64(1))
	c.Assert(len(hg.Buckets), Equals, 1)
}

func (s *testStatsUpdateSuite) TestOutOfOrderUpdate(c *C) {
	defer cleanEnv(c, s.store, s.do)
	testKit := testkit.NewTestKit(c, s.store)
	testKit.MustExec("use test")
	testKit.MustExec("create table t (a int, b int)")
	testKit.MustExec("insert into t values (1,2)")

	do := s.do
	is := do.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tableInfo := tbl.Meta()
	h := do.StatsHandle()
	h.HandleDDLEvent(<-h.DDLEventCh())

	// Simulate the case that another tidb has inserted some value, but delta info has not been dumped to kv yet.
	testKit.MustExec("insert into t values (2,2),(4,5)")
	c.Assert(h.DumpStatsDeltaToKV(), IsNil)
	testKit.MustExec(fmt.Sprintf("update mysql.stats_meta set count = 1 where table_id = %d", tableInfo.ID))

	testKit.MustExec("delete from t")
	c.Assert(h.DumpStatsDeltaToKV(), IsNil)
	testKit.MustQuery("select count from mysql.stats_meta").Check(testkit.Rows("1"))

	// Now another tidb has updated the delta info.
	testKit.MustExec(fmt.Sprintf("update mysql.stats_meta set count = 3 where table_id = %d", tableInfo.ID))

	c.Assert(h.DumpStatsDeltaToKV(), IsNil)
	testKit.MustQuery("select count from mysql.stats_meta").Check(testkit.Rows("0"))
}
