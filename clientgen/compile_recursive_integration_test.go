//go:build integration

package clientgen_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"trest.local/trest"
	"trest.local/trest/clientgen"
)

type treeModel struct {
	Label    string                `json:"label"`
	Next     *treeModel            `json:"next,omitempty"`
	Children []treeModel           `json:"children"`
	ByName   map[string]*treeModel `json:"by_name"`
}

func TestGeneratedRecursiveClientsRoundTrip(t *testing.T) {
	app, err := trest.New(trest.Post("/tree", func(_ context.Context, tree treeModel) (treeModel, error) { return tree, nil }).Name("tree"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	t.Setenv("TREST_RECURSIVE_URL", server.URL)
	t.Run("Go", func(t *testing.T) {
		source, err := clientgen.Go(document, "generated")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		put(t, dir, "go.mod", []byte("module generated.test/tree\n\ngo 1.23.0\n"))
		put(t, dir, "client.go", source)
		put(t, dir, "client_test.go", []byte(recursiveGoClientTest))
		run(t, dir, "go", "test", "-race", "./...")
		put(t, dir, "invalid.go", []byte("package generated\nfunc invalid(tree TreeOutput){tree.Next.Label=123}\n"))
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("compiler accepted wrong recursive field type: %s", out)
		}
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
		put(t, dir, "runtime.ts", []byte(recursiveTSClientTest))
		run(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "runtime.ts")
		run(t, dir, "node", "runtime.js")
	})
}

const recursiveGoClientTest = `package generated
import("context";"encoding/json";"os";"testing")
func TestTreeRoundTrip(t *testing.T){
 client,err:=NewClient(os.Getenv("TREST_RECURSIVE_URL"),nil);if err!=nil{t.Fatal(err)}
 var input TreeInput
 if err=json.Unmarshal([]byte(` + "`" + `{"label":"root","next":{"label":"next","children":null,"by_name":null},"children":[{"label":"child","children":[],"by_name":{}}],"by_name":{"leaf":{"label":"leaf","children":null,"by_name":null}}}` + "`" + `),&input.Body);err!=nil{t.Fatal(err)}
 result,err:=client.Tree(context.Background(),input);if err!=nil{t.Fatal(err)}
 if result.Status!=200||result.Data.Label!="root"||result.Data.Next==nil||result.Data.Next.Label!="next"{t.Fatalf("bad linked response: %#v",result)}
 if result.Data.Children==nil||len(*result.Data.Children)!=1||(*result.Data.Children)[0].Label!="child"{t.Fatalf("bad recursive slice: %#v",result.Data.Children)}
 if result.Data.ByName==nil||(*result.Data.ByName)["leaf"].Label!="leaf"{t.Fatalf("bad recursive map: %#v",result.Data.ByName)}
}
`

const recursiveTSClientTest = `import {Client} from './client';
declare const process: {env:Record<string,string|undefined>;exitCode:number};
async function main(){
 const client=new Client(process.env.TREST_RECURSIVE_URL!);
 const result=await client.tree({body:{label:'root',next:{label:'next',children:null,by_name:null},children:[{label:'child',children:[],by_name:{}}],by_name:{leaf:{label:'leaf',children:null,by_name:null}}}});
 if(result.status!==200||result.data.label!=='root'||result.data.next?.label!=='next'||result.data.children?.[0].label!=='child'||result.data.by_name?.leaf?.label!=='leaf')throw new Error('recursive round trip failed');
 if(false){
 // @ts-expect-error recursive labels retain their string type
 client.tree({body:{label:'root',children:[],by_name:null,next:{label:123,children:null,by_name:null}}});
 }
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
