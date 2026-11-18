import assert from "node:assert/strict";
import { test } from "node:test";
import { requestWithPolicy, decideRequest, refusedExecution } from "../index.js";
import { harness, moduleFor } from "./helpers.js";

test("negotiated worker callback retains immutable scope and never sends account auth",async()=>{
  const module=moduleFor(async(ctx,_call,stream)=>decideRequest(ctx.signal,stream,{step:"read",facts:{status:401,operation:"tool.call",phase:"response",complete:true},rules:[],evidence:""}));
  module.manifest.contract.version.minor=12;
  module.manifest.contract.capabilities.push("request-policy");
  const worker=harness(module);
  const scope={plugin:"fixture",provider:"selected",account:"account",generation:3,operation:"original",attempt:1};
  try{
    worker.send({kind:"call",id:"configure",method:"component.configure",params:{component:"primary",instance:"instance",contract:{},config:{version:"1",values:{}}}});
    assert.equal((await worker.received.next()).error,undefined);
    worker.send({kind:"call",id:"parent",method:"operation.status",scope,auth:{token:"synthetic-private"}});
    const callback=await worker.received.next();
    assert.equal(callback.kind,"host.call");assert.equal(callback.method,"request.recover");
    assert.deepEqual(callback.scope,scope);assert.equal(callback.auth,undefined);
    assert.equal(JSON.stringify(callback).includes("synthetic-private"),false);
    const decision={retry:false,reason:"unsafe",class:"request-refusal",scope:"request",rule:"invalid",attempt:0};
    worker.send({kind:"host.result",id:callback.id,parent:callback.parent,result:decision});
    assert.deepEqual((await worker.received.next()).result,decision);
  }finally{await worker.close();}
});

test("request policy repeats only the same request after host permission", async () => {
  const signal = new AbortController().signal;
  const request = new Request("https://fixture.invalid/generate", {method:"POST",body:"same business bytes"});
  let sends = 0;
  let decisions = 0;
  const host = { async hostCall(method, facts) {
    assert.equal(method,"request.recover");
    assert.equal(facts.facts.status,429);
    assert.equal(facts.step,"generate");
    decisions++;
    return {step:"generate",action:decisions===1?"wait":"terminal",retry:decisions===1,class:"quota",scope:"account",rule:"quota",reason:"test",attempt:decisions-1};
  }};
  const result = await requestWithPolicy(signal,host,"generate",request,async (sent,options) => {
    sends++;
    assert.equal(options.redirect,"manual");
    assert.equal(sent.url,request.url);
    assert.equal(sent.method,"POST");
    assert.equal(await sent.text(),"same business bytes");
    return new Response("refused",{status:429});
  },() => ({facts:{operation:"provider.execute",phase:"response",complete:true},evidence:"rejected-before-effect",rules:[]}));
  assert.equal(sends,2);
  assert.equal(decisions,2);
  assert.equal(result.decision.class,"quota");
  assert.equal(await result.response.text(),"refused");
});

test("classification without evidence still calls the host; transport failure never does", async () => {
  const signal = new AbortController().signal;
  let decisions = 0;
  const host = {async hostCall(_method,facts) {
    decisions++;
    assert.equal(facts.evidence,"");
    return {retry:false,class:"request-refusal",scope:"request",rule:"request",reason:"unsafe",attempt:0};
  }};
  const request = new Request("https://fixture.invalid/search");
  const classify = () => ({facts:{operation:"module.call",phase:"response",complete:true},evidence:"",rules:[]});
  const result = await requestWithPolicy(signal,host,"search",request,async()=>new Response("bad request",{status:401}),classify);
  assert.equal(result.decision.class,"request-refusal");
  const disconnected = new Error("connection lost");
  await assert.rejects(requestWithPolicy(signal,host,"search",request,async()=>{throw disconnected;},classify), error=>error===disconnected);
  assert.equal(decisions,1);
});

test("canceled request cannot ask for permission",async()=>{
  const controller=new AbortController();controller.abort();
  await assert.rejects(decideRequest(controller.signal,{hostCall(){assert.fail("host called");}},{}));
});

test("final refusal preserves fresh typed timing and window applicability on the wire", async () => {
  const failure = {step:"inference",facts:{status:200,code:"budget",message:"window exhausted",operation:"provider.execute",phase:"response",complete:true,retryAfterMs:42000,resetAt:"2026-11-15T12:00:42Z"},rules:[{id:"budget",code:"budget",class:"quota",scope:"model"}],evidence:"rejected-before-effect"};
  const windows = [{id:"free-window",models:["free"],resetAt:"2026-11-15T12:00:42Z"}];
  const module = moduleFor(async()=>refusedExecution(failure,windows));
  module.manifest.contract.version.minor=12;
  module.manifest.contract.capabilities.push("request-policy");
  const worker=harness(module);
  try {
    worker.send({kind:"call",id:"configure",method:"component.configure",params:{component:"primary",instance:"instance",contract:{},config:{version:"1",values:{}}}});
    assert.equal((await worker.received.next()).error,undefined);
    worker.send({kind:"call",id:"final",method:"operation.status",scope:{plugin:"fixture"}});
    const reply=await worker.received.next();
    assert.equal(reply.error,undefined);
    assert.equal(reply.result.status,200);
    assert.equal(reply.result.outcome,"not-started");
    assert.deepEqual(reply.result.refusal,{...failure,windows});
  } finally {await worker.close();}
});
