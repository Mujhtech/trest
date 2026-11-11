//go:build integration

package clientgen_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/clientgen"
)

type variantRequest struct {
	Queued bool `query:"queued" default:"false"`
}

type createdResponse struct {
	ID string `json:"id"`
}

type queuedResponse struct {
	Ticket string `json:"ticket"`
}

func TestGeneratedStatusVariantClientsRoundTrip(t *testing.T) {
	app, err := trest.New(
		trest.Get("/result", func(_ context.Context, input variantRequest) (trest.Either[createdResponse, queuedResponse], error) {
			if input.Queued {
				return trest.Second[createdResponse, queuedResponse](queuedResponse{Ticket: "job-1"}).WithHeader("X-Result", "queued"), nil
			}
			return trest.First[createdResponse, queuedResponse](createdResponse{ID: "created"}).WithHeader("X-Result", "created"), nil
		}).Name("result").Statuses(201, 202),
		trest.Get("/same", func(_ context.Context, input variantRequest) (trest.Response[createdResponse], error) {
			status := 201
			if input.Queued {
				status = 202
			}
			return trest.JSON(status, createdResponse{ID: "same"}), nil
		}).Name("same").Statuses(201, 202),
	)
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	t.Setenv("TREST_VARIANT_URL", server.URL)
	t.Run("Go", func(t *testing.T) {
		source, err := clientgen.Go(document, "generated")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		put(t, dir, "go.mod", []byte("module generated.test/variant\n\ngo 1.23.0\n"))
		put(t, dir, "client.go", source)
		put(t, dir, "client_test.go", []byte(statusGoClientTest))
		run(t, dir, "go", "test", "-race", "./...")
	})
	t.Run("TypeScript", func(t *testing.T) {
		tsc := os.Getenv("TREST_TSC")
		if tsc == "" {
			tsc, _ = exec.LookPath("tsc")
		}
		if tsc == "" {
			t.Skip("set TREST_TSC to run TypeScript compiler verification")
		}
		source, err := clientgen.TypeScript(document)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		put(t, dir, "client.ts", source)
		put(t, dir, "runtime.ts", []byte(statusTSClientTest))
		run(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "runtime.ts")
		run(t, dir, "node", "runtime.js")
	})
}

const statusGoClientTest = `package generated
import("context";"os";"testing")
func TestStatusRoundTrip(t *testing.T){
 client,err:=NewClient(os.Getenv("TREST_VARIANT_URL"),nil);if err!=nil{t.Fatal(err)}
 result,err:=client.Result(context.Background(),ResultInput{});if err!=nil{t.Fatal(err)}
 created,ok:=result.As201();if !ok||created.Id!="created"||result.Status202!=nil||result.Header.Get("X-Result")!="created"{t.Fatalf("created branch: %#v",result)}
 queued:=true;result,err=client.Result(context.Background(),ResultInput{Query:ResultQuery{Queued:&queued}});if err!=nil{t.Fatal(err)}
 pending,ok:=result.As202();if !ok||pending.Ticket!="job-1"||result.Status201!=nil||result.Header.Get("X-Result")!="queued"{t.Fatalf("queued branch: %#v",result)}
 same,err:=client.Same(context.Background(),SameInput{Query:SameQuery{Queued:&queued}});if err!=nil||same.Status!=202||same.Data.Id!="same"{t.Fatalf("same-body compatibility: %#v %v",same,err)}
}
`

const statusTSClientTest = `import {Client} from './client';
declare const process:{env:Record<string,string|undefined>;exitCode:number};
async function main(){
 const client=new Client(process.env.TREST_VARIANT_URL!);
 const created=await client.result();
 if(created.status!==201||created.data.id!=='created'||created.headers.get('X-Result')!=='created')throw new Error('created variant failed');
 const queued=await client.result({query:{queued:true}});
 if(queued.status!==202||queued.data.ticket!=='job-1'||queued.headers.get('X-Result')!=='queued')throw new Error('queued variant failed');
 const same=await client.same({query:{queued:true}});
 if(same.status!==202||same.data.id!=='same')throw new Error('same-body compatibility failed');
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
