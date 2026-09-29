package snoozebot

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v62/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimOwnershipRenewReleaseAndReclaim(t *testing.T){db,err:=InitDB(t.TempDir()+"/claims.db");require.NoError(t,err);defer func(){_=db.Close()}();app:=&App{DB:db,claimLeaseDuration:50*time.Millisecond};require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(-time.Hour),10));base:=time.Now();claimed,err:=app.claimSnooze(1,base,"a");require.NoError(t,err);assert.True(t,claimed);owned,err:=app.renewSnoozeClaimOnce(1,"a",base.Add(10*time.Millisecond));require.NoError(t,err);assert.True(t,owned);owned,err=app.renewSnoozeClaimOnce(1,"stale",base.Add(10*time.Millisecond));require.NoError(t,err);assert.False(t,owned);claimed,err=app.claimSnooze(1,base.Add(70*time.Millisecond),"b");require.NoError(t,err);assert.True(t,claimed);released,err:=app.releaseSnooze(1,"a");require.NoError(t,err);assert.False(t,released);assert.Error(t,app.deleteSnoozeIfOwned(1,"a"));released,err=app.releaseSnooze(1,"b");require.NoError(t,err);assert.True(t,released)}

func TestRenewalLoopCancelsOnOwnershipLoss(t *testing.T){db,err:=InitDB(t.TempDir()+"/loss.db");require.NoError(t,err);defer func(){_=db.Close()}();app:=&App{DB:db,claimLeaseDuration:time.Second,claimRenewInterval:5*time.Millisecond};require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(-time.Hour),10));require.NoError(t,setClaimOwner(db,1,"other"));cancelled:=make(chan struct{},1);ctx,cancel:=context.WithCancel(context.Background());defer cancel();go app.renewSnoozeClaimLoop(ctx,1,"a",func(){select{case cancelled<-struct{}{}:default:}});select{case <-cancelled:case <-time.After(250*time.Millisecond):t.Fatal("renewal loop did not cancel")}}

func TestRenewalLoopCancelsOnDatabaseFailure(t *testing.T){db,err:=InitDB(t.TempDir()+"/dbfail.db");require.NoError(t,err);app:=&App{DB:db,claimRenewInterval:5*time.Millisecond};require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(-time.Hour),10));claimed,err:=app.claimSnooze(1,time.Now(),"a");require.NoError(t,err);require.True(t,claimed);require.NoError(t,db.Close());cancelled:=make(chan struct{},1);ctx,cancel:=context.WithCancel(context.Background());defer cancel();go app.renewSnoozeClaimLoop(ctx,1,"a",func(){select{case cancelled<-struct{}{}:default:}});select{case <-cancelled:case <-time.After(250*time.Millisecond):t.Fatal("renewal loop did not cancel after db failure")}}

func TestPostSnoozeReplyCancellationReachesHTTPRequest(t *testing.T){db,err:=InitDB(t.TempDir()+"/cancel.db");require.NoError(t,err);defer func(){_=db.Close()}();app:=&App{DB:db,claimRenewInterval:5*time.Millisecond};require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(-time.Hour),10));require.NoError(t,setClaimOwner(db,1,"replacement"));cancelled:=make(chan struct{},1);srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){select{case <-r.Context().Done():cancelled<-struct{}{};return;case <-time.After(time.Second):http.Error(w,"timeout",504)}}));defer srv.Close();client:=github.NewClient(srv.Client());base,err:=url.Parse(srv.URL+"/");require.NoError(t,err);client.BaseURL=base;app.clientOverride=func(int64)(*github.Client,error){return client,nil};err=app.PostSnoozeReply(context.Background(),SnoozeRecord{ID:1,RepoOwner:"owner",RepoName:"repo",IssueID:1,Username:"alice",InstallationID:10},"a");assert.Error(t,err);select{case <-cancelled:case <-time.After(250*time.Millisecond):t.Fatal("request did not observe cancellation")}}

func TestPermissionFailurePreservesReminderAndEvictsAuth(t *testing.T){db,err:=InitDB(t.TempDir()+"/permission.db");require.NoError(t,err);defer func(){_=db.Close()}();require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(-time.Hour),10));srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){http.Error(w,"forbidden",403)}));defer srv.Close();client:=github.NewClient(srv.Client());base,err:=url.Parse(srv.URL+"/");require.NoError(t,err);client.BaseURL=base;app:=&App{DB:db,appTransports:map[int64]*ghinstallation.Transport{10:{}},clientOverride:func(int64)(*github.Client,error){return client,nil}};app.CheckExpiredSnoozes(context.Background());var count int;require.NoError(t,db.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&count));assert.Equal(t,1,count);var owner,locked sql.NullString;require.NoError(t,db.QueryRow(`SELECT claim_owner,locked_until FROM snoozes LIMIT 1`).Scan(&owner,&locked));assert.False(t,owner.Valid);assert.False(t,locked.Valid);_,ok:=app.appTransports[10];assert.False(t,ok)}

func TestPendingReminderSurvivesDatabaseReopen(t *testing.T){p:=t.TempDir()+"/restart.db";db,err:=InitDB(p);require.NoError(t,err);require.NoError(t,InsertSnooze(db,"owner","repo",1,"alice",time.Now().Add(time.Hour),10));require.NoError(t,db.Close());db,err=InitDB(p);require.NoError(t,err);defer func(){_=db.Close()}();var n int;require.NoError(t,db.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&n));assert.Equal(t,1,n)}

func setClaimOwner(db *sql.DB,id int,owner string) error{_,err:=db.Exec(`UPDATE snoozes SET claim_owner=?,locked_until=? WHERE id=?`,owner,time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),id);return err}
