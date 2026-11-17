// 隔离容器中的合成冷缓存检查；不访问真实 CCloud，不执行CLI版本命令。
import http from 'node:http';
import {execFile} from 'node:child_process';
import {homedir} from 'node:os';
import {readdirSync,readFileSync} from 'node:fs';
import {join} from 'node:path';
if(process.getuid()!==10001||homedir()!=='/tmp/sre-control'||process.env.HOME)throw Error('unexpected user/home');
const base='/cloud/api/v1/agent';let calls=0;
const server=http.createServer((req,res)=>{
 calls++;if(req.headers.authorization!=='Bearer synthetic-narrow-token')throw Error('bad fixture token');
 let data;
 if(req.url===base+'/meta/resources')data={resources:[{name:'app',summary:'synthetic'}]};
 else if(req.url===base+'/meta/resources/app/actions')data={resource:'app',version:1,actions:[{name:'pods',description:'synthetic',method:'GET',pathTemplate:base+'/app/pods',requiresAuth:true,dangerous:false,executionMode:'HTTP_JSON',arguments:[{name:'appId',description:'synthetic',location:'query',type:'string',required:true,defaultValue:null,itemType:null,fileInputEnabled:false},{name:'clusterId',description:'synthetic',location:'query',type:'number',required:true,defaultValue:null,itemType:null,fileInputEnabled:false}]}]};
 else if(req.url===base+'/app/pods?appId=synthetic&clusterId=60')data=[];
 else{res.writeHead(404);res.end();return;}
 res.writeHead(200,{'Content-Type':'application/json'});res.end(JSON.stringify({code:0,message:'',data}));
});
server.listen(0,'127.0.0.1',()=>{
 execFile('/usr/local/bin/node',['/opt/ccloud-cli/dist/ccloud-cli.js','app','pods','--app-id','synthetic','--cluster-id','60'],{env:{PATH:'/usr/local/bin:/usr/bin:/bin',CCLOUD_API_KEY:'synthetic-narrow-token',CCLOUD_API_BASE_URL:`http://127.0.0.1:${server.address().port}`,CCLOUD_API_MODE:'real',CCLOUD_DISABLE_UPDATE_CHECK:'1'},timeout:30000,maxBuffer:256*1024},(err,stdout)=>{
  try{
   if(err||!Array.isArray(JSON.parse(stdout).data)||calls!==3)throw Error('cold cache chain failed');
   const dir='/tmp/sre-control/.ccloud/cache';const files=readdirSync(dir,{recursive:true});
   if(!files.length)throw Error('metadata cache absent');
   for(const name of files){try{if(readFileSync(join(dir,name),'utf8').includes('synthetic-narrow-token'))throw Error('token cached');}catch(e){if(e.code!=='EISDIR')throw e;}}
   console.log(JSON.stringify({passed:true,uid:10001,node:process.version,home_from_passwd:true,cache_written:true,cold_cache_requests:3}));
  }catch{console.error('isolated CLI cache smoke failed');process.exitCode=1;}
  finally{server.close();}
 });
});
