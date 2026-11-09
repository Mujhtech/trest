//go:build integration

package clientgen_test

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"trest.local/trest"
)

type headerArrays struct {
	Labels []string `header:"X-Label" json:"labels"`
	Counts []int    `header:"X-Count" json:"counts"`
	Flags  []bool   `header:"X-Flag" json:"flags"`
}

func TestGeneratedHeaderArraysRoundTrip(t *testing.T) {
	var calls atomic.Int32
	app, err := trest.New(trest.Get("/headers", func(_ context.Context, input headerArrays) (headerArrays, error) { calls.Add(1); return input, nil }).Name("headerArrays"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	t.Setenv("TREST_HEADERS_URL", server.URL)
	clients := exerciseGeneratedClients(t, document, headerGoTest, headerTSTest)
	if calls.Load() != int32(clients) {
		t.Fatalf("expected one valid request per client, received %d", calls.Load())
	}
}

const headerGoTest = `package generated
import("context";"os";"testing")
func TestHeaderArrays(t *testing.T){
 client,err:=NewClient(os.Getenv("TREST_HEADERS_URL"),nil);if err!=nil{t.Fatal(err)}
 labels:=[]string{"alpha","beta"};counts:=[]int64{1,2};flags:=[]bool{true,false}
 input:=HeaderArraysInput{Headers:HeaderArraysHeaders{XLabel:&labels,XCount:&counts,XFlag:&flags}}
 result,err:=client.HeaderArrays(context.Background(),input);if err!=nil{t.Fatal(err)}
 if result.Data.Labels==nil||len(*result.Data.Labels)!=2||(*result.Data.Labels)[1]!="beta"||result.Data.Counts==nil||(*result.Data.Counts)[1]!=2||result.Data.Flags==nil||(*result.Data.Flags)[1]{t.Fatalf("header arrays changed: %#v",result.Data)}
 for _,invalid:=range []string{"ambiguous,value","bad\r\nheader"," leading","trailing ","control\x00"}{labels=[]string{invalid};if _,err:=client.HeaderArrays(context.Background(),input);err==nil{t.Fatalf("invalid header item accepted: %q",invalid)}}
}
`

const headerTSTest = `import {Client} from './client';
declare const process:{env:Record<string,string|undefined>;exitCode:number};
async function main(){
 const client=new Client(process.env.TREST_HEADERS_URL!);
 const result=await client.headerArrays({headers:{'X-Label':['alpha','beta'],'X-Count':[1,2],'X-Flag':[true,false]}});
 if(result.data.labels?.join(',')!=='alpha,beta'||result.data.counts?.[1]!==2||result.data.flags?.[1]!==false)throw new Error('fetch header arrays changed');
 for(const invalid of ['ambiguous,value','bad\r\nheader',' leading','trailing ','control\0']){
  let failed=false;try{await client.headerArrays({headers:{'X-Label':[invalid],'X-Count':[1],'X-Flag':[true]}});}catch{failed=true;}if(!failed)throw new Error('invalid header item accepted');
 }
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
