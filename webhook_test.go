package snoozebot

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func signWebhook(payload []byte,secret string) string{m:=hmac.New(sha256.New,[]byte(secret));_,_=m.Write(payload);return "sha256="+hex.EncodeToString(m.Sum(nil))}
func webhookRequest(event string,payload []byte,secret,delivery string,signed bool)*http.Request{r:=httptest.NewRequest(http.MethodPost,"/webhook",bytes.NewReader(payload));r.Header.Set("Content-Type","application/json");r.Header.Set("X-GitHub-Event",event);if delivery!=""{r.Header.Set("X-GitHub-Delivery",delivery)};if signed{r.Header.Set("X-Hub-Signature-256",signWebhook(payload,secret))};return r}
func fakeGitHubUserClient(t *testing.T,status int,location string)*github.Client{t.Helper();srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.URL.Path!="/users/alice"{http.NotFound(w,r);return};w.Header().Set("Content-Type","application/json");w.WriteHeader(status);if status>=200&&status<300{_ = json.NewEncoder(w).Encode(map[string]any{"login":"alice","location":location})}else{_ = json.NewEncoder(w).Encode(map[string]any{"message":"test error"})}}));t.Cleanup(srv.Close);c:=github.NewClient(srv.Client());base,err:=url.Parse(srv.URL+"/");require.NoError(t,err);c.BaseURL=base;return c}
func issueCommentPayload(t *testing.T,action,body,userType string,installationID int64)[]byte{t.Helper();e:=&github.IssueCommentEvent{Action:github.String(action),Issue:&github.Issue{Number:github.Int(7)},Comment:&github.IssueComment{Body:github.String(body),User:&github.User{Login:github.String("alice"),Type:github.String(userType)}},Repo:&github.Repository{Name:github.String("repo"),FullName:github.String("owner/repo"),Owner:&github.User{Login:github.String("owner")}},Sender:&github.User{Login:github.String("alice")}};if installationID>0{e.Installation=&github.Installation{ID:github.Int64(installationID)}};b,err:=json.Marshal(e);require.NoError(t,err);return b}
func newWebhookTestApp(t *testing.T,path string)*App{t.Helper();db,err:=InitDB(path);require.NoError(t,err);t.Cleanup(func(){_=db.Close()});client:=fakeGitHubUserClient(t,http.StatusOK,"UTC");return &App{Config:Config{GitHubAppID:123,GitHubAppIDRaw:"123",GitHubAppPrivateKeyFile:"unused",WebhookSecret:"secret",BotCommand:"@snooze"},DB:db,clientOverride:func(id int64)(*github.Client,error){if id<=0{return nil,fmt.Errorf("missing installation")};return client,nil}}}

func TestWebhookSignatureValidation(t *testing.T){payload:=[]byte(`{"zen":"testing"}`);app:=&App{Config:Config{GitHubAppID:123,WebhookSecret:"secret"}};for _,tc:=range []struct{name string;req *http.Request;want int}{{"valid",webhookRequest("ping",payload,"secret","p1",true),200},{"invalid",func()*http.Request{r:=webhookRequest("ping",payload,"secret","p2",true);r.Header.Set("X-Hub-Signature-256","sha256=deadbeef");return r}(),400},{"missing",webhookRequest("ping",payload,"secret","p3",false),400}}{t.Run(tc.name,func(t *testing.T){rr:=httptest.NewRecorder();app.handleWebhook(rr,tc.req);assert.Equal(t,tc.want,rr.Code)})}}

func TestWebhookIssueCommentIdempotencyAndRestart(t *testing.T){path:=t.TempDir()+"/webhook.db";app:=newWebhookTestApp(t,path);payload:=issueCommentPayload(t,"created","@snooze tomorrow","User",10);for i:=0;i<2;i++{rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",payload,"secret","delivery-1",true));assert.Equal(t,200,rr.Code)};var n int;require.NoError(t,app.DB.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&n));assert.Equal(t,1,n);require.NoError(t,app.DB.Close());db,err:=InitDB(path);require.NoError(t,err);t.Cleanup(func(){_=db.Close()});app.DB=db;app.clientOverride=func(int64)(*github.Client,error){return fakeGitHubUserClient(t,200,"UTC"),nil};rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",payload,"secret","delivery-1",true));assert.Equal(t,200,rr.Code);require.NoError(t,db.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&n));assert.Equal(t,1,n)}

func TestWebhookIssueCommentStatusMatrix(t *testing.T){tests:=[]struct{name,action,body,userType,delivery string;installationID int64;wantStatus,wantRows int}{{"missing delivery","created","@snooze tomorrow","User","",10,400,0},{"malformed time","created","@snooze definitely-not-a-date","User","d-bad",10,400,0},{"edited ignored","edited","@snooze tomorrow","User","d-edit",10,200,0},{"bot ignored","created","@snooze tomorrow","Bot","d-bot",10,200,0},{"created persists","created","@snooze tomorrow","User","d-ok",10,200,1}};for _,tt:=range tests{t.Run(tt.name,func(t *testing.T){app:=newWebhookTestApp(t,t.TempDir()+"/matrix.db");rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",issueCommentPayload(t,tt.action,tt.body,tt.userType,tt.installationID),"secret",tt.delivery,true));assert.Equal(t,tt.wantStatus,rr.Code);var n int;require.NoError(t,app.DB.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&n));assert.Equal(t,tt.wantRows,n)})}}

func TestWebhookMissingInstallationAndAuthFailureAreRetryable(t *testing.T){secret:="secret";db,err:=InitDB(t.TempDir()+"/auth.db");require.NoError(t,err);defer func(){_=db.Close()}();app:=&App{Config:Config{GitHubAppID:123,GitHubAppIDRaw:"123",GitHubAppPrivateKeyFile:"unused",WebhookSecret:secret,BotCommand:"@snooze"},DB:db};for _,id:=range []int64{0,10}{rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",issueCommentPayload(t,"created","@snooze tomorrow","User",id),secret,fmt.Sprintf("auth-%d",id),true));assert.Equal(t,500,rr.Code)};var n int;require.NoError(t,db.QueryRow(`SELECT COUNT(*) FROM snoozes`).Scan(&n));assert.Equal(t,0,n)}

func TestWebhookStorageFailureReturnsServerError(t *testing.T){db,err:=InitDB(t.TempDir()+"/closed.db");require.NoError(t,err);client:=fakeGitHubUserClient(t,200,"UTC");app:=&App{Config:Config{GitHubAppID:123,WebhookSecret:"secret",BotCommand:"@snooze"},DB:db,clientOverride:func(int64)(*github.Client,error){return client,nil}};require.NoError(t,db.Close());rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",issueCommentPayload(t,"created","@snooze tomorrow","User",10),"secret","storage",true));assert.Equal(t,500,rr.Code)}

func TestWebhookLocationAuthFailureEvictsCachedTransport(t *testing.T){db,err:=InitDB(t.TempDir()+"/loc.db");require.NoError(t,err);defer func(){_=db.Close()}();client:=fakeGitHubUserClient(t,403,"");app:=&App{Config:Config{GitHubAppID:123,WebhookSecret:"secret",BotCommand:"@snooze"},DB:db,appTransports:map[int64]*ghinstallation.Transport{10:{}},clientOverride:func(int64)(*github.Client,error){return client,nil}};rr:=httptest.NewRecorder();app.handleWebhook(rr,webhookRequest("issue_comment",issueCommentPayload(t,"created","@snooze tomorrow","User",10),"secret","loc",true));assert.Equal(t,500,rr.Code);_,ok:=app.appTransports[10];assert.False(t,ok)}

func TestProcessedDeliveryRetentionUsesUTC(t *testing.T){app:=newWebhookTestApp(t,t.TempDir()+"/retention.db");require.NoError(t,app.insertSnoozeIdempotent("utc","owner","repo",1,"alice",time.Now().Add(time.Hour),10));var processed string;require.NoError(t,app.DB.QueryRow(`SELECT processed_at FROM processed_deliveries WHERE delivery_id='utc'`).Scan(&processed));_,err:=time.Parse(time.RFC3339,processed);assert.NoError(t,err)}
