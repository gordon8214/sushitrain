// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Included inside the provider namespace: all filesystem changes are serialized
// here, while hydration callbacks run on independent workers.
struct LocalFile {
    fs::path path;
    std::wstring key, stamp;
    bool directory{};
    JsonObject identity;
    CF_IN_SYNC_STATE sync{CF_IN_SYNC_STATE_NOT_IN_SYNC};
    CF_PIN_STATE pin{CF_PIN_STATE_UNSPECIFIED};
    USN usn{};
};
std::vector<JsonObject> records;
std::unordered_map<std::wstring,handle> captureLocks;
std::wstring captureDirectory;
std::set<int64_t> remoteItems;
std::unordered_map<int64_t,size_t> itemIndex;
bool checkpointDirty{};
uint64_t projectedRevision=MAXUINT64;
void indexRecords();
bool checkpointLoaded{};
int64_t item(JsonObject const& entry) { return static_cast<int64_t>(entry.GetNamedNumber(L"Item",0)); }
void indexRecords() {itemIndex.clear();for(size_t i=0;i<records.size();++i)itemIndex[item(records[i].GetNamedObject(L"Entry"))]=i;}
std::wstring rootRelative(fs::path const& path) { return path.lexically_relative(rootPath).generic_wstring(); }
handle attributes(fs::path const& path) {
    handle file(CreateFileW(windowsPath(path).c_str(), FILE_READ_ATTRIBUTES | FILE_WRITE_ATTRIBUTES | WRITE_DAC,
        FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, nullptr, OPEN_EXISTING,
        FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS, nullptr));
    if (file.get()==INVALID_HANDLE_VALUE) throw_last_error(); return file;
}
LocalFile inspect(fs::path const& path,HANDLE supplied=nullptr) {
    LocalFile result; result.path=path;
    handle file; if (!supplied) file=attributes(path); auto raw=supplied ? supplied : file.get(); BY_HANDLE_FILE_INFORMATION basic{};
    check_bool(GetFileInformationByHandle(raw,&basic));
    result.directory=(basic.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY)!=0;
    result.key=std::to_wstring(basic.dwVolumeSerialNumber)+L"-"+std::to_wstring((uint64_t(basic.nFileIndexHigh)<<32)|basic.nFileIndexLow);
    result.stamp=std::to_wstring((uint64_t(basic.ftLastWriteTime.dwHighDateTime)<<32)|basic.ftLastWriteTime.dwLowDateTime)+L"-"+std::to_wstring((uint64_t(basic.nFileSizeHigh)<<32)|basic.nFileSizeLow);
    if (basic.nNumberOfLinks>1 && !result.directory) throw hresult_error(E_INVALIDARG,L"Hard links cannot be synchronized through Cloud Files.");
    if (basic.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) {
        std::vector<std::byte> buffer(8192); auto info=reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(buffer.data());
        check_hresult(CfGetPlaceholderInfo(raw,CF_PLACEHOLDER_INFO_STANDARD,info,static_cast<DWORD>(buffer.size()),nullptr));
        result.identity=JsonObject::Parse(to_hstring(std::string(reinterpret_cast<char*>(info->FileIdentity),info->FileIdentityLength)));
        if (result.identity.GetNamedString(L"ClientID",L"")!=clientID) throw hresult_error(E_ACCESSDENIED,L"A placeholder belongs to another local index. Its content has been preserved.");
        result.sync=info->InSyncState; result.pin=info->PinState;
    }
    std::array<std::byte,512> journal{}; DWORD returned{};
    if (DeviceIoControl(raw,FSCTL_READ_FILE_USN_DATA,nullptr,0,journal.data(),static_cast<DWORD>(journal.size()),&returned,nullptr)) {
        auto record=reinterpret_cast<USN_RECORD_V2*>(journal.data());
        if (returned>=sizeof(USN_RECORD_V2) && record->MajorVersion==2) result.usn=record->Usn;
    }
    return result;
}
int64_t projectedItemAt(fs::path const& path) {
    auto flags=GetFileAttributesW(windowsPath(path).c_str());
    if(flags==INVALID_FILE_ATTRIBUTES || !(flags&FILE_ATTRIBUTE_REPARSE_POINT))return 0;
    return item(inspect(path).identity);
}
void retainRemote(JsonObject const& entry,bool subtree) {
    remoteItems.insert(item(entry));
    if(!subtree || entry.GetNamedNumber(L"Kind",0)==0)return;
    auto found=itemIndex.find(item(entry));if(found==itemIndex.end())return;
    auto prefix=std::wstring(records[found->second].GetNamedString(L"Path"))+L"/";
    for(auto const& record:records)if(std::wstring(record.GetNamedString(L"Path")).starts_with(prefix))remoteItems.insert(item(record.GetNamedObject(L"Entry")));
}
bool samePlaceholder(HANDLE file,JsonObject const& entry) {
    std::vector<std::byte> buffer(8192);auto info=reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(buffer.data());
    if(FAILED(CfGetPlaceholderInfo(file,CF_PLACEHOLDER_INFO_STANDARD,info,static_cast<DWORD>(buffer.size()),nullptr)))return false;
    auto current=JsonObject::Parse(to_hstring(std::string(reinterpret_cast<char*>(info->FileIdentity),info->FileIdentityLength)));
    return current.GetNamedString(L"ClientID",L"")==clientID && item(current)==item(entry) && current.GetNamedNumber(L"Node",0)==entry.GetNamedNumber(L"Node",0);
}
void saveCheckpoint() {
    JsonArray values; for (auto const& record: records) values.Append(record);
    JsonObject args; args.Insert(L"Snapshot",values); request(L"projection.save",args);checkpointDirty=false;
}
JsonObject entryAt(fs::path const& path) {
    if (path==fs::path(rootPath)) { JsonObject root; root.Insert(L"Item",number(0)); return root; }
    JsonObject args; args.Insert(L"Path",text(rootRelative(path))); return request(L"files.resolve",args).result.GetObject();
}
USN finishUSN(HANDLE file) {
    USN usn{};DWORD returned{};check_bool(DeviceIoControl(file,FSCTL_WRITE_USN_CLOSE_RECORD,nullptr,0,&usn,sizeof(usn),&returned,nullptr));
    if(returned!=sizeof(usn) || !usn)throw hresult_error(E_FAIL,L"NTFS change tracking is unavailable for this location.");
    return usn;
}
USN bind(LocalFile const& local, JsonObject const& entry,HANDLE captureHandle=nullptr) {
    auto id=to_string(entry.Stringify()); if (id.size()>4096) throw hresult_error(E_BOUNDS);
    auto file=attributes(local.path);
    if(inspect(local.path,file.get()).key!=local.key)throw hresult_error(E_FAIL,L"A file was replaced while recovering its cloud identity. Its content has been preserved.");
    USN usn{};
    if (item(local.identity)==0) check_hresult(CfConvertToPlaceholder(file.get(),id.data(),static_cast<DWORD>(id.size()),CF_CONVERT_FLAG_NONE,&usn,nullptr));
    else check_hresult(CfUpdatePlaceholder(file.get(),nullptr,id.data(),static_cast<DWORD>(id.size()),nullptr,0,CF_UPDATE_FLAG_NONE,&usn,nullptr));
    // Finish NTFS's pending metadata record while writers are still excluded.
    // Otherwise closing the capture handle appends a new CLOSE USN, which can
    // look like another user save and indefinitely postpone the settle timer.
    // Flush the capture's file object last: its rename/read reasons survive a
    // CLOSE record written through a separate metadata handle.
    if(captureHandle)file.close();
    usn=finishUSN(captureHandle?captureHandle:file.get());
    return usn;
}
void observe(JsonObject& record,LocalFile const& local) {
    record.Insert(L"Path",text(rootRelative(local.path))); record.Insert(L"Key",text(local.key)); record.Insert(L"Stamp",text(local.stamp)); record.Insert(L"USN",text(std::to_wstring(local.usn)));
}
JsonObject capture(LocalFile const& local,JsonObject const& entry,HANDLE existing=nullptr) {
    if (item(local.identity)!=0) {
        auto file=attributes(local.path); LARGE_INTEGER offset{},length{}; length.QuadPart=CF_EOF;
        check_hresult(CfHydratePlaceholder(file.get(),offset,length,CF_HYDRATE_FLAG_NONE,nullptr));
    }
    // Deny concurrent writes and replacement while making a complete immutable
    // copy. The host never reads inside its own Cloud Files location.
    HANDLE raw{};
    if(existing)check_bool(DuplicateHandle(GetCurrentProcess(),existing,GetCurrentProcess(),&raw,0,FALSE,DUPLICATE_SAME_ACCESS));
    else raw=CreateFileW(windowsPath(local.path).c_str(),GENERIC_READ|DELETE|FILE_WRITE_ATTRIBUTES,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_SEQUENTIAL_SCAN,nullptr);
    handle input(raw);
    if (input.get()==INVALID_HANDLE_VALUE) throw_last_error();
    FILETIME unchanged{MAXDWORD,MAXDWORD};check_bool(SetFileTime(input.get(),nullptr,&unchanged,nullptr));
    LARGE_INTEGER beginning{};check_bool(SetFilePointerEx(input.get(),beginning,nullptr,FILE_BEGIN));
    GUID guid{}; check_hresult(CoCreateGuid(&guid)); wchar_t uuid[40]{}; StringFromGUID2(guid,uuid,40);
    auto name=std::wstring(uuid)+L".snapshot"; auto target=fs::path(captureDirectory)/name;
    handle output(CreateFileW(windowsPath(target).c_str(),GENERIC_WRITE,0,nullptr,CREATE_NEW,FILE_ATTRIBUTE_NORMAL,nullptr));
    if (output.get()==INVALID_HANDLE_VALUE) throw_last_error();
    try {
        std::vector<std::byte> buffer(1024*1024); for (;;) { DWORD read{}; check_bool(ReadFile(input.get(),buffer.data(),static_cast<DWORD>(buffer.size()),&read,nullptr)); if (!read) break; writeAll(output.get(),buffer.data(),read); }
        FILETIME modified{}; check_bool(GetFileTime(input.get(),nullptr,nullptr,&modified)); check_bool(SetFileTime(output.get(),nullptr,nullptr,&modified)); check_bool(FlushFileBuffers(output.get()));
    } catch (...) { output.close(); DeleteFileW(windowsPath(target).c_str()); throw; }
    output.close();
    handle parentDirectory(CreateFileW(windowsPath(captureDirectory).c_str(),GENERIC_READ|GENERIC_WRITE,FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE,nullptr,OPEN_EXISTING,FILE_FLAG_BACKUP_SEMANTICS,nullptr));
    if(parentDirectory.get()==INVALID_HANDLE_VALUE)throw_last_error();check_bool(FlushFileBuffers(parentDirectory.get()));
    auto args=itemArguments(entry); args.Insert(L"Captured",text(name)); args.Insert(L"ClientID",text(clientID));
    args.Insert(L"Parent",number(item(entryAt(local.path.parent_path())))); args.Insert(L"Name",text(local.path.filename().wstring()));
    args.Insert(L"Path",text(rootRelative(local.path)));
    args.Insert(L"CaptureUSN",text(std::to_wstring(inspect(local.path).usn)));
    captureLocks.emplace(name,std::move(input));
    return args;
}
void finishCapture(JsonObject& record, LocalFile const& local) {
    auto pending=record.GetNamedObject(L"Capture");
    if(!pending.HasKey(L"LocalFolder"))pending.Insert(L"LocalFolder",record.GetNamedObject(L"Entry").GetNamedValue(L"Folder",number(0)));
    auto name=std::wstring(pending.GetNamedString(L"Captured"));
    if (!captureLocks.contains(name)) {
        handle input(CreateFileW(windowsPath(local.path).c_str(),GENERIC_READ|DELETE|FILE_WRITE_ATTRIBUTES,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT,nullptr));
        if (input.get()==INVALID_HANDLE_VALUE) throw_last_error(); FILETIME unchanged{MAXDWORD,MAXDWORD};check_bool(SetFileTime(input.get(),nullptr,&unchanged,nullptr));captureLocks.emplace(name,std::move(input));
    }
    bool changed=std::to_wstring(inspect(local.path).usn)!=pending.GetNamedString(L"CaptureUSN",L"");
    auto result=request(L"files.stage",pending).result.GetObject();
    auto entry=result.GetNamedObject(L"Entry"); entry.Insert(L"ClientID",text(clientID));
    if (result.GetNamedBoolean(L"Removed",false)) {
        // The prior capture still excludes writers and DELETE opens. Duplicate
        // that same file object instead of reopening a competing capture lock.
        auto replacement=capture(local,entry,captureLocks.at(name).get()); record.Insert(L"Capture",replacement); saveCheckpoint();
        request(L"files.stageAck",pending);captureLocks.erase(name);return;
    }
    auto path=fs::path(rootPath)/std::wstring(result.GetNamedString(L"Path"));
    auto actual=local;
    if (path!=local.path && result.GetNamedBoolean(L"Conflict",false)) {
        // Rename using the capture's own DELETE-capable handle, retaining its
        // writer/replacement exclusion through binding and checkpoint/ACK.
        auto target=fs::absolute(path).make_preferred().wstring();std::vector<std::byte> buffer(offsetof(FILE_RENAME_INFO,FileName)+(target.size()+1)*sizeof(wchar_t));auto rename=reinterpret_cast<FILE_RENAME_INFO*>(buffer.data());
        rename->ReplaceIfExists=FALSE;rename->RootDirectory=nullptr;rename->FileNameLength=static_cast<DWORD>(target.size()*sizeof(wchar_t));memcpy(rename->FileName,target.data(),rename->FileNameLength);
        if(!SetFileInformationByHandle(captureLocks.at(name).get(),FileRenameInfo,rename,static_cast<DWORD>(buffer.size()))){auto error=HRESULT_FROM_WIN32(GetLastError());throw hresult_error(error,L"Renaming a captured conflict: "+hresult_error(error).message());}actual.path=path;
        changed=changed || inspect(path,captureLocks.at(name).get()).stamp!=record.GetNamedString(L"Stamp");
    }
    auto boundUSN=bind(actual,entry,captureLocks.at(name).get());
    auto capturedLocal=inspect(actual.path);record.Insert(L"Key",text(capturedLocal.key));record.Insert(L"Stamp",text(capturedLocal.stamp));
    record.Insert(L"USN",text(std::to_wstring(boundUSN)));
    record.Insert(L"NeedsCapture",JsonValue::CreateBooleanValue(changed));
    // Save the identity and capture receipt before ACK. A lost response replays
    // the same receipt without allocating another item or content version.
    record.Insert(L"Entry",entry); record.Insert(L"Path",text(std::wstring(result.GetNamedString(L"Path")))); record.Insert(L"AwaitingSync",JsonValue::CreateBooleanValue(true));
    record.Remove(L"Capture"); record.Insert(L"Ack",pending); saveCheckpoint();
    request(L"files.stageAck",pending); record.Remove(L"Ack"); saveCheckpoint(); captureLocks.erase(name);
}
std::wstring operationID() { GUID guid{};check_hresult(CoCreateGuid(&guid));wchar_t value[40]{};StringFromGUID2(guid,value,40);return value; }
void finishCreate(JsonObject& record,LocalFile const& local) {
    auto pending=record.GetNamedObject(L"Create");
    auto entry=request(L"files.create",pending).result.GetObject();entry.Insert(L"ClientID",text(clientID));
    auto usn=bind(local,entry);record.Insert(L"Entry",entry);record.Insert(L"USN",text(std::to_wstring(usn)));record.Insert(L"AwaitingSync",JsonValue::CreateBooleanValue(true));
    record.Remove(L"Create");record.Insert(L"CreateAck",pending);saveCheckpoint();request(L"files.createAck",pending);record.Remove(L"CreateAck");saveCheckpoint();
}

void scan(fs::path const& directory,std::vector<LocalFile>& out) {
    for (auto const& child:fs::directory_iterator(directory)) {
        auto local=inspect(child.path()); out.push_back(local);
        if (local.directory) scan(child.path(),out);
    }
}
bool clean(JsonObject const& entry) {
    JsonObject args; args.Insert(L"Item",number(item(entry)));
    auto state=request(L"files.state",args).result.GetString();
    return state==L"remote" || state==L"partial" || state==L"local" || state==L"kept" || state.empty();
}
void renameLocal(JsonObject& record,LocalFile const& local) {
    if(record.GetNamedString(L"Path")==rootRelative(local.path))return;
    auto entry=record.GetNamedObject(L"Entry");
    if (entry.GetNamedNumber(L"Kind")==3) throw hresult_error(E_ACCESSDENIED,L"Use Synctrain to remove a remote folder.");
    auto old=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
    auto args=itemArguments(entry); args.Insert(L"Parent",number(item(entryAt(old.parent_path())))); args.Insert(L"Name",text(old.filename().wstring())); args.Insert(L"NewParent",number(item(entryAt(local.path.parent_path())))); args.Insert(L"Path",text(local.path.filename().wstring()));
    request(L"files.rename",args);
    auto previous=rootRelative(old), next=rootRelative(local.path);
    for (auto& child:records) { auto childPath=std::wstring(child.GetNamedString(L"Path")); if (childPath.starts_with(previous+L"/")) child.Insert(L"Path",text(next+childPath.substr(previous.size()))); }
    record.Insert(L"Path",text(rootRelative(local.path)));saveCheckpoint();
}
void localChanges() {
    std::vector<LocalFile> locals; scan(rootPath,locals);
    std::set<size_t> seen;
    std::unordered_map<std::wstring,size_t> keys,paths;
    auto indexLocals=[&]{keys.clear();paths.clear();for(size_t i=0;i<records.size();++i){keys[std::wstring(records[i].GetNamedString(L"Key",L""))]=i;paths[std::wstring(records[i].GetNamedString(L"Path",L""))]=i;}};
    indexLocals();
    for (auto const& local:locals) {
        size_t index=records.size();
        if(auto found=keys.find(local.key);found!=keys.end())index=found->second;
        else if(auto foundPath=paths.find(rootRelative(local.path));foundPath!=paths.end())index=foundPath->second;
        if (index==records.size()) {
            JsonObject record; auto entry=local.identity; entry.Insert(L"ClientID",text(clientID));
            if (item(entry)==0) {
                entry.Insert(L"Kind",number(local.directory ? 1 : 0));

            }
            record.Insert(L"Entry",entry); observe(record,local);
            if (local.directory && item(entry)==0) {
                JsonObject args;args.Insert(L"Parent",number(item(entryAt(local.path.parent_path()))));args.Insert(L"Name",text(local.path.filename().wstring()));args.Insert(L"Directory",JsonValue::CreateBooleanValue(true));args.Insert(L"ID",text(operationID()));args.Insert(L"ClientID",text(clientID));record.Insert(L"Create",args);
            } else if (!local.directory && item(entry)==0) record.Insert(L"Capture",capture(local,entry));
            records.push_back(record); index=records.size()-1;keys[local.key]=index;paths[rootRelative(local.path)]=index;
            if(record.HasKey(L"Create") || record.HasKey(L"Capture"))saveCheckpoint();else checkpointDirty=true;
        }
        seen.insert(index); if(records[index].HasKey(L"Detaching")) records[index].Remove(L"Detaching"); auto& record=records[index]; auto entry=record.GetNamedObject(L"Entry");
        if (record.HasKey(L"Create")) {finishCreate(record,local);continue;}
        if (record.HasKey(L"CreateAck")) {request(L"files.createAck",record.GetNamedObject(L"CreateAck"));record.Remove(L"CreateAck");saveCheckpoint();}
        if (record.HasKey(L"Ack")) { request(L"files.stageAck",record.GetNamedObject(L"Ack")); record.Remove(L"Ack"); saveCheckpoint(); }
        if (record.HasKey(L"Capture")) { finishCapture(record,local); continue; }
        if (record.GetNamedString(L"Path")!=rootRelative(local.path)) {renameLocal(record,local);indexLocals();}
        auto changedStamp=record.GetNamedString(L"Stamp",L"")!=local.stamp;
        auto changedUSN=record.GetNamedString(L"USN",L"")!=std::to_wstring(local.usn);
        if (!local.directory && (item(entry)==0 || record.GetNamedBoolean(L"NeedsCapture",false) || changedStamp || (local.sync==CF_IN_SYNC_STATE_NOT_IN_SYNC && (changedUSN || (!record.GetNamedBoolean(L"AwaitingSync",false) && clean(entry)))))) {
            record.Insert(L"Capture",capture(local,entry)); observe(record,local); saveCheckpoint(); finishCapture(record,local); continue;
        }
        if (item(entry)!=0 && local.sync!=CF_IN_SYNC_STATE_IN_SYNC && clean(entry)) {
            handle file(CreateFileW(windowsPath(local.path).c_str(),FILE_READ_ATTRIBUTES|FILE_WRITE_ATTRIBUTES|WRITE_DAC,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
            if(file.get()==INVALID_HANDLE_VALUE){rescan=true;continue;}
            auto current=inspect(local.path,file.get());
            if(current.key!=local.key || item(current.identity)!=item(entry) || current.identity.GetNamedNumber(L"Node",0)!=entry.GetNamedNumber(L"Node",0) || (!local.directory && (record.GetNamedString(L"USN",L"0")!=std::to_wstring(current.usn) || record.GetNamedString(L"Stamp",L"")!=current.stamp))){rescan=true;continue;}
            // CFAPI's nonzero InSyncUsn fails for NOT_IN_SYNC placeholders on
            // supported Windows versions. Exclude writers/replacement and prove
            // the captured version ourselves before setting the state.
            check_hresult(CfSetInSyncState(file.get(),CF_IN_SYNC_STATE_IN_SYNC,CF_SET_IN_SYNC_FLAG_NONE,nullptr));
            record.Insert(L"AwaitingSync",JsonValue::CreateBooleanValue(false));record.Insert(L"USN",text(std::to_wstring(finishUSN(file.get()))));checkpointDirty=true;
        }
        bool pinned=local.pin==CF_PIN_STATE_PINNED;
        if (record.HasKey(L"Pinned") && record.GetNamedBoolean(L"Pinned")!=pinned) {
            auto args=itemArguments(entry);
            // A removed selection can disappear between this scan and a pin
            // notification. Its stale notification must not block cleanup.
            if(request(L"files.state",args).result.GetString()!=L"removed"){
                args.Insert(L"Enabled",JsonValue::CreateBooleanValue(pinned));request(L"files.keep",args);
            }
        }
        record.Insert(L"Pinned",JsonValue::CreateBooleanValue(pinned));
    }
    // Children first, so directory deletion never hides outstanding file edits.
    for (size_t n=records.size();n>0;--n) {
        auto i=n-1; if (seen.contains(i)) continue;
        auto record=records[i]; auto entry=record.GetNamedObject(L"Entry");
        if (record.GetNamedBoolean(L"Detaching",false) || record.GetNamedBoolean(L"RemovedRemotely",false)) {records.erase(records.begin()+static_cast<ptrdiff_t>(i));saveCheckpoint();continue;}
        if (record.HasKey(L"Capture")) continue; // Never discard an unacknowledged capture.
        if (entry.GetNamedNumber(L"Kind",0)==3) continue;
        auto old=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
        auto args=itemArguments(entry); args.Insert(L"Parent",number(item(entryAt(old.parent_path())))); args.Insert(L"Name",text(old.filename().wstring())); args.Insert(L"Directory",JsonValue::CreateBooleanValue(entry.GetNamedNumber(L"Kind",0)!=0));
        try { request(L"files.delete",args); } catch (...) {
            // A repeated delete after a lost response is complete only if the
            // shared index agrees the old path is absent.
            JsonObject resolve; resolve.Insert(L"Path",text(rootRelative(old)));
            try { request(L"files.resolve",resolve); throw hresult_error(E_FAIL,L"The removed item changed remotely; review it before deleting it."); }
            catch (hresult_error const& error) { if (std::wstring(error.message()).find(L"file does not exist")==std::wstring::npos) throw; }
        }
        records.erase(records.begin()+static_cast<ptrdiff_t>(i)); saveCheckpoint();
    }
}
void projected(fs::path const& path,JsonObject const& entry) {
    auto local=inspect(path);
    if(auto found=itemIndex.find(item(entry));found!=itemIndex.end()) {auto& record=records[found->second];
        if (record.HasKey(L"Capture")) return;
        if(record.GetNamedBoolean(L"AwaitingSync",false))return;
        record.Insert(L"Entry",entry); observe(record,local); record.Insert(L"Pinned",JsonValue::CreateBooleanValue(entry.GetNamedBoolean(L"Keep",false))); checkpointDirty=true; return;
    }
    JsonObject record; record.Insert(L"Entry",entry); observe(record,local); record.Insert(L"Pinned",JsonValue::CreateBooleanValue(entry.GetNamedBoolean(L"Keep",false))); records.push_back(record);itemIndex[item(entry)]=records.size()-1;checkpointDirty=true;
}
bool moveRemote(JsonObject const& entry,fs::path const& path) {
    remoteItems.insert(item(entry));
    if(auto found=itemIndex.find(item(entry));found!=itemIndex.end()) {auto& record=records[found->second];
        auto old=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
        if (old==path || GetFileAttributesW(windowsPath(old).c_str())==INVALID_FILE_ATTRIBUTES) return true;
        auto local=inspect(old);
        if (record.HasKey(L"Capture") || local.sync!=CF_IN_SYNC_STATE_IN_SYNC || !clean(record.GetNamedObject(L"Entry"))) return false;
        JsonObject intent;intent.Insert(L"From",text(rootRelative(old)));intent.Insert(L"To",text(rootRelative(path)));record.Insert(L"RemoteMove",intent);saveCheckpoint();
        handle guard(CreateFileW(windowsPath(old).c_str(),FILE_READ_ATTRIBUTES|DELETE,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
        if (guard.get()==INVALID_HANDLE_VALUE) return false;
        auto protectedLocal=inspect(old,guard.get());
        if (protectedLocal.key!=record.GetNamedString(L"Key") || protectedLocal.sync!=CF_IN_SYNC_STATE_IN_SYNC) return false;
        auto target=fs::absolute(path).make_preferred().wstring();std::vector<std::byte> buffer(offsetof(FILE_RENAME_INFO,FileName)+(target.size()+1)*sizeof(wchar_t));auto rename=reinterpret_cast<FILE_RENAME_INFO*>(buffer.data());
        rename->ReplaceIfExists=FALSE;rename->RootDirectory=nullptr;rename->FileNameLength=static_cast<DWORD>(target.size()*sizeof(wchar_t));memcpy(rename->FileName,target.data(),rename->FileNameLength);
        check_bool(SetFileInformationByHandle(guard.get(),FileRenameInfo,rename,static_cast<DWORD>(buffer.size())));
        guard.close(); record.Remove(L"RemoteMove");
        auto previous=rootRelative(old), next=rootRelative(path);
        for (auto& child:records) {
            auto childPath=std::wstring(child.GetNamedString(L"Path"));
            if (childPath==previous || childPath.starts_with(previous+L"/")) child.Insert(L"Path",text(next+childPath.substr(previous.size())));
        }
        saveCheckpoint(); return true;
    }
    return true;
}
void removeRemote() {
    bool intents=false;
    for(auto& record:records)if(!remoteItems.contains(item(record.GetNamedObject(L"Entry"))) && !record.HasKey(L"Capture")){record.Insert(L"RemovedRemotely",JsonValue::CreateBooleanValue(true));intents=true;}
    if(intents)saveCheckpoint();
    for (size_t n=records.size();n>0;--n) {
        auto i=n-1; auto record=records[i]; if (remoteItems.contains(item(record.GetNamedObject(L"Entry"))) || record.HasKey(L"Capture")) continue;
        auto path=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
        if (GetFileAttributesW(windowsPath(path).c_str())!=INVALID_FILE_ATTRIBUTES) {
            handle guard(CreateFileW(windowsPath(path).c_str(),FILE_READ_ATTRIBUTES|DELETE,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
            if (guard.get()==INVALID_HANDLE_VALUE) continue;
            auto local=inspect(path,guard.get());
            auto entry=record.GetNamedObject(L"Entry");
            if (local.key!=record.GetNamedString(L"Key") || item(local.identity)!=item(entry) || local.identity.GetNamedNumber(L"Node",0)!=entry.GetNamedNumber(L"Node",0) || (!local.directory && (local.sync!=CF_IN_SYNC_STATE_IN_SYNC || local.stamp!=record.GetNamedString(L"Stamp")))) continue;
            // Child removal changes directory timestamps/in-sync state. NTFS
            // still refuses to remove a directory containing any local file.
            FILE_DISPOSITION_INFO_EX disposition{};disposition.Flags=FILE_DISPOSITION_FLAG_DELETE|FILE_DISPOSITION_FLAG_IGNORE_READONLY_ATTRIBUTE;
            if (!SetFileInformationByHandle(guard.get(),FileDispositionInfoEx,&disposition,sizeof(disposition))) continue;
        }
        records.erase(records.begin()+static_cast<ptrdiff_t>(i));checkpointDirty=true;
    }
}
// Directories are recovered parent-first before any durable child capture is
// replayed. This restores the shared namespace used by captures and conflicts.
void recoverParents(std::vector<LocalFile> const& observed) {
    for(auto const& local:observed)if(local.directory){
        auto current=inspect(local.path);if(current.key!=local.key)throw hresult_error(E_FAIL,L"A directory changed during recovery. Try again after saving finishes.");
        auto found=std::find_if(records.begin(),records.end(),[&](JsonObject const& record){return record.GetNamedString(L"Key")==local.key;});
        if(found==records.end()){
            auto parent=item(entryAt(current.path.parent_path()));
            if(!parent)throw hresult_error(E_INVALIDARG,L"Use Synctrain to select a gateway folder. Local files at the Explorer root have been preserved.");
            JsonObject record,entry;entry.Insert(L"Kind",number(1));entry.Insert(L"ClientID",text(clientID));record.Insert(L"Entry",entry);observe(record,current);
            JsonObject args;args.Insert(L"Parent",number(parent));args.Insert(L"Name",text(current.path.filename().wstring()));args.Insert(L"Directory",JsonValue::CreateBooleanValue(true));args.Insert(L"ID",text(operationID()));args.Insert(L"ClientID",text(clientID));record.Insert(L"Create",args);
            records.push_back(record);found=std::prev(records.end());saveCheckpoint();
        }
        if(found->HasKey(L"Create")){finishCreate(*found,current);current=inspect(current.path);}
        auto entry=found->GetNamedObject(L"Entry");if(!item(entry))throw hresult_error(E_FAIL,L"A directory creation has not been acknowledged. Its files have been preserved.");
        if(item(current.identity)==0){auto usn=bind(current,entry);found->Insert(L"USN",text(std::to_wstring(usn)));found->Insert(L"AwaitingSync",JsonValue::CreateBooleanValue(true));saveCheckpoint();}
        renameLocal(*found,current);
    }
}
void reconcile() {
    auto status=request(L"status").result.GetObject();
    if (status.GetNamedString(L"mode",L"")!=L"files" || status.GetNamedString(L"clientID",L"")!=clientID) return;
    auto revision=static_cast<uint64_t>(status.GetNamedNumber(L"revision"));
    bool quiescing=!quiescePath.empty() && fs::exists(quiescePath);
    if(checkpointLoaded && revision==projectedRevision && !rescan.exchange(false) && !quiescing)return;
    captureDirectory=status.GetNamedString(L"captureDir");
    if (!checkpointLoaded) {
        auto saved=request(L"projection.load").result;
        if (saved && saved.ValueType()==JsonValueType::Array) for (auto const& value:saved.GetArray()) {
            auto record=value.GetObject(); if (record.GetNamedObject(L"Entry").GetNamedString(L"ClientID",L"")==clientID) records.push_back(record);
        }
        checkpointLoaded=true;
    }
    for (auto& record:records) if (record.HasKey(L"RemoteMove")) {
        auto intent=record.GetNamedObject(L"RemoteMove");auto target=fs::path(rootPath)/std::wstring(intent.GetNamedString(L"To"));
        if (GetFileAttributesW(windowsPath(target).c_str())==INVALID_FILE_ATTRIBUTES) continue;
        auto local=inspect(target);if (local.key!=record.GetNamedString(L"Key")) throw hresult_error(E_FAIL,L"A move destination changed while Synctrain was stopped.");
        auto from=std::wstring(intent.GetNamedString(L"From")),to=std::wstring(intent.GetNamedString(L"To"));
        for (auto& child:records) {auto childPath=std::wstring(child.GetNamedString(L"Path"));if (childPath==from || childPath.starts_with(from+L"/")) child.Insert(L"Path",text(to+childPath.substr(from.size())));}
        record.Remove(L"RemoteMove");saveCheckpoint();
    }
    // Captures are durable operations even if their physical file was removed
    // while the provider was stopped. Import/ack them before observing deletion.
    for (auto& record:records) if (record.HasKey(L"Create")) {
        auto path=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
        if (GetFileAttributesW(windowsPath(path).c_str())!=INVALID_FILE_ATTRIBUTES) continue;
        auto pending=record.GetNamedObject(L"Create");auto entry=request(L"files.create",pending).result.GetObject();entry.Insert(L"ClientID",text(clientID));record.Insert(L"Entry",entry);record.Remove(L"Create");record.Insert(L"CreateAck",pending);saveCheckpoint();request(L"files.createAck",pending);record.Remove(L"CreateAck");saveCheckpoint();
    }
    std::vector<LocalFile> observed;scan(rootPath,observed);
    if(recoveringRegistration)recoverParents(observed);
    for (auto& record:records) if (record.HasKey(L"Capture")) {
        auto found=std::find_if(observed.begin(),observed.end(),[&](LocalFile const& local){return local.key==record.GetNamedString(L"Key");});
        if (found!=observed.end()) {finishCapture(record,*found);continue;}
        auto path=fs::path(rootPath)/std::wstring(record.GetNamedString(L"Path"));
        if (GetFileAttributesW(windowsPath(path).c_str())!=INVALID_FILE_ATTRIBUTES) continue;
        auto pending=record.GetNamedObject(L"Capture");if(!pending.HasKey(L"LocalFolder"))pending.Insert(L"LocalFolder",record.GetNamedObject(L"Entry").GetNamedValue(L"Folder",number(0)));auto result=request(L"files.stage",pending).result.GetObject();
        auto entry=result.GetNamedObject(L"Entry");entry.Insert(L"ClientID",text(clientID));record.Insert(L"Entry",entry);record.Insert(L"Path",result.GetNamedValue(L"Path"));record.Remove(L"Capture");record.Insert(L"Ack",pending);saveCheckpoint();request(L"files.stageAck",pending);record.Remove(L"Ack");saveCheckpoint();
    }
    if(recoveringRegistration){
        observed.clear();scan(rootPath,observed);
        // MSIX unregistration can remove dehydrated placeholders. Recreate
        // those from the shared index; only durable captures/receipts above are
        // imported first. Existing physical files still follow normal capture.
        for(auto it=records.begin();it!=records.end();){
            auto path=fs::path(rootPath)/std::wstring(it->GetNamedString(L"Path"));
            auto surviving=std::find_if(observed.begin(),observed.end(),[&](LocalFile const& local){return local.key==it->GetNamedString(L"Key");});
            if(surviving!=observed.end()){
                auto current=inspect(surviving->path);
                if(current.key==it->GetNamedString(L"Key") && item(current.identity)==0){
                    auto entry=it->GetNamedObject(L"Entry");
                    if(current.directory){auto usn=bind(current,entry);it->Insert(L"USN",text(std::to_wstring(usn)));it->Insert(L"AwaitingSync",JsonValue::CreateBooleanValue(true));saveCheckpoint();}
                    else {auto pending=capture(current,entry);pending.Insert(L"Recovery",JsonValue::CreateBooleanValue(true));it->Insert(L"Capture",pending);saveCheckpoint();finishCapture(*it,current);}
                }
                ++it;continue;
            }
            if(GetFileAttributesW(windowsPath(path).c_str())!=INVALID_FILE_ATTRIBUTES){++it;continue;}
            auto error=GetLastError();if(error!=ERROR_FILE_NOT_FOUND && error!=ERROR_PATH_NOT_FOUND)throw_last_error();
            if(it->HasKey(L"Ack"))request(L"files.stageAck",it->GetNamedObject(L"Ack"));
            if(it->HasKey(L"CreateAck"))request(L"files.createAck",it->GetNamedObject(L"CreateAck"));
            it=records.erase(it);
        }
        saveCheckpoint();if(!testRegistration)saveRootOwnership(false);recoveringRegistration=false;
    }
    localChanges();
    indexRecords();
    remoteItems.clear();
    projectionIssues=JsonArray();
    projectDirectory(0,rootPath);
    removeRemote();
    auto issues=std::wstring(projectionIssues.Stringify());
    if(issues!=reportedIssues){JsonObject args;args.Insert(L"Snapshot",projectionIssues);request(L"projection.report",args);reportedIssues=issues;}
    if(checkpointDirty)saveCheckpoint();
    projectedRevision=revision;
}

// Called only after the host quiesced captures and drained the shared outbox.
// Approval is a proof about each indexed version, never an inference from a
// native icon. Hold every native object against writes/replacement before any
// removal; interference leaves the connected location and all local bytes intact.
void detachLocation() {
    std::ifstream stream{fs::path(approvalPath)};std::string data((std::istreambuf_iterator<char>(stream)),std::istreambuf_iterator<char>());
    auto approval=JsonObject::Parse(to_hstring(data));
    if (approval.GetNamedString(L"ClientID")!=clientID) throw hresult_error(E_ACCESSDENIED,L"The local index changed while switching modes.");
    std::set<std::pair<int64_t,int64_t>> approved;
    for (auto const& value:approval.GetNamedArray(L"Clean")) {auto entry=value.GetObject();approved.emplace(item(entry),static_cast<int64_t>(entry.GetNamedNumber(L"Node")));}
    std::vector<LocalFile> locals;scan(rootPath,locals);
    struct Guard {LocalFile local;handle file;};std::vector<Guard> guards;
    for (auto const& local:locals) {
        auto found=std::find_if(records.begin(),records.end(),[&](JsonObject const& record){return record.GetNamedString(L"Key")==local.key;});
        if (found==records.end() || found->HasKey(L"Capture") || found->HasKey(L"Create") || found->GetNamedBoolean(L"NeedsCapture",false)) throw hresult_error(E_FAIL,L"Local files still need to be captured. Finish saving them, then switch modes again.");
        auto entry=found->GetNamedObject(L"Entry");
        if (!approved.contains({item(entry),static_cast<int64_t>(entry.GetNamedNumber(L"Node",0))})) throw hresult_error(E_FAIL,L"A local file is pending, changed remotely, or local-only. Let it synchronize or export it before switching modes.");
        handle guard(CreateFileW(windowsPath(local.path).c_str(),FILE_READ_ATTRIBUTES|WRITE_DAC|DELETE,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
        if (guard.get()==INVALID_HANDLE_VALUE) throw hresult_error(E_FAIL,L"A file is open for writing. Close it before switching modes.");
        auto current=inspect(local.path,guard.get());
        if (current.key!=local.key) throw hresult_error(E_FAIL,L"A file was replaced while switching modes. Try again after saving finishes.");
        if (current.sync!=CF_IN_SYNC_STATE_IN_SYNC) {
            auto saved=static_cast<USN>(std::stoll(std::wstring(found->GetNamedString(L"USN",L"0"))));
            if (!found->GetNamedBoolean(L"AwaitingSync",false) || !saved || current.usn!=saved) throw hresult_error(E_FAIL,L"A file changed while switching modes. Let it synchronize, then try again.");
            check_hresult(CfSetInSyncState(guard.get(),CF_IN_SYNC_STATE_IN_SYNC,CF_SET_IN_SYNC_FLAG_NONE,nullptr));
        }
        guards.push_back({current,std::move(guard)});
    }
    // Children before parents. New files created during cleanup keep directories
    // nonempty, so Windows refuses their removal and the mode change is aborted.
    for (auto it=guards.rbegin();it!=guards.rend();++it) {
        FILE_DISPOSITION_INFO_EX disposition{};disposition.Flags=FILE_DISPOSITION_FLAG_DELETE|FILE_DISPOSITION_FLAG_IGNORE_READONLY_ATTRIBUTE;
        check_bool(SetFileInformationByHandle(it->file.get(),FileDispositionInfoEx,&disposition,sizeof(disposition)));it->file.close();
    }
    if (!fs::is_empty(rootPath)) throw hresult_error(E_FAIL,L"New files appeared while switching modes. They have been preserved; try again after saving finishes.");
}

// Legacy packages did not persist a root file ID. Recover only a complete,
// unchanged inventory whose NTFS identities the protected checkpoint already
// bound to this client. Never adopt an arbitrary nonempty directory.
bool legacyOwnsRoot() {
    auto snapshot=request(L"projection.load").result;if(!snapshot || snapshot.ValueType()!=JsonValueType::Array)return false;
    std::unordered_map<std::wstring,std::wstring> expected;
    for(auto const& value:snapshot.GetArray()){
        auto record=value.GetObject();if(record.GetNamedObject(L"Entry").GetNamedString(L"ClientID",L"")!=clientID)return false;
        if(!expected.emplace(std::wstring(record.GetNamedString(L"Path")),std::wstring(record.GetNamedString(L"Key"))).second)return false;
    }
    size_t matched{};
    for(auto const& child:fs::recursive_directory_iterator(rootPath)){
        auto relative=rootRelative(child.path());auto found=expected.find(relative);if(found==expected.end())return false;
        handle file(CreateFileW(windowsPath(child.path()).c_str(),FILE_READ_ATTRIBUTES,FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
        if(file.get()==INVALID_HANDLE_VALUE)return false;BY_HANDLE_FILE_INFORMATION info{};if(!GetFileInformationByHandle(file.get(),&info))return false;
        auto key=std::to_wstring(info.dwVolumeSerialNumber)+L"-"+std::to_wstring((uint64_t(info.nFileIndexHigh)<<32)|info.nFileIndexLow);
        if(key!=found->second)return false;++matched;
    }
    return matched>0 && matched==expected.size();
}
