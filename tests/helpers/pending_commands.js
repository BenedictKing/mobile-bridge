'use strict';
const {PendingCommands}=require('../../web/pending-commands.js');
class MemoryStorage{
  constructor(){this.values=new Map();}
  get length(){return this.values.size;}
  key(i){return [...this.values.keys()][i];}
  getItem(key){return this.values.get(key)??null;}
  setItem(key,value){this.values.set(key,value);}
  removeItem(key){this.values.delete(key);}
}
module.exports={PendingCommands,MemoryStorage};
