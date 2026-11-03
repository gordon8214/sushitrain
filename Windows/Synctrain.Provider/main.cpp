// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
#include <windows.h>
#include <winioctl.h>
#include <objbase.h>
#include <shobjidl.h>
#include <shlwapi.h>
#include <shellapi.h>
#include <thumbcache.h>
#include <wincodec.h>
#include <array>
#include <winternl.h>
#include <cfapi.h>
#include <sddl.h>
#include <winrt/Windows.Foundation.h>
#include <winrt/Windows.Foundation.Collections.h>
#include <winrt/Windows.Data.Json.h>
#include <winrt/Windows.Storage.h>
#include <winrt/Windows.Storage.Provider.h>
#include <winrt/Windows.Security.Cryptography.h>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <thread>
#include <mutex>
#include <condition_variable>
#include <deque>
#include <functional>
#include <unordered_map>
#include <atomic>
#include <set>
#include <map>
#include <algorithm>

using namespace winrt;
using namespace Windows::Data::Json;
using namespace Windows::Storage;
using namespace Windows::Storage::Provider;
using namespace Windows::Security::Cryptography;
namespace fs = std::filesystem;

namespace {
std::wstring windowsPath(fs::path const& value) {
    auto path=fs::absolute(value).lexically_normal().native();
    if(path.starts_with(LR"(\\?\)"))return path;
    if(path.starts_with(LR"(\\)"))return LR"(\\?\UNC\)"+path.substr(2);
    return LR"(\\?\)"+path;
}
std::wstring rootPath, pipePath, clientID, shutdownPath, quiescePath, detachPath, approvalPath;
bool testRegistration{};
bool recoveringRegistration{};
std::wstring rootKey;
JsonArray projectionIssues{nullptr};
std::wstring reportedIssues;
void projected(fs::path const& path,JsonObject const& entry);
handle attributes(fs::path const& path);
bool moveRemote(JsonObject const& entry,fs::path const& path);
int64_t item(JsonObject const& entry);
int64_t projectedItemAt(fs::path const& path);
void retainRemote(JsonObject const& entry,bool subtree);
bool samePlaceholder(HANDLE file,JsonObject const& entry);
bool legacyOwnsRoot();
CF_CONNECTION_KEY connection{};
std::atomic<bool> stopped{};
std::mutex jobsMutex, changesMutex;
std::condition_variable jobsReady;
std::deque<std::function<void()>> jobs;
std::unordered_map<LONGLONG, std::shared_ptr<std::atomic<bool>>> cancellations;
std::atomic<bool> rescan{true};

struct ProtectedFile {
    HANDLE value{};
    explicit ProtectedFile(fs::path const& path) { check_hresult(CfOpenFileWithOplock(windowsPath(path).c_str(),CF_OPEN_FILE_FLAG_EXCLUSIVE,&value)); }
    ~ProtectedFile() { if (value) CfCloseHandle(value); }
    ProtectedFile(ProtectedFile const&)=delete; ProtectedFile& operator=(ProtectedFile const&)=delete;
};
JsonValue number(int64_t value) { return JsonValue::CreateNumberValue(static_cast<double>(value)); }
JsonValue text(std::wstring const& value) { return JsonValue::CreateStringValue(value); }
void writeAll(HANDLE file, void const* data, DWORD size) {
    auto bytes = static_cast<char const*>(data);
    while (size) { DWORD written{}; check_bool(WriteFile(file, bytes, size, &written, nullptr)); if (!written) throw hresult_error(E_FAIL); bytes += written; size -= written; }
}
JsonObject itemArguments(JsonObject const& entry) {
    JsonObject args;
    for (auto name:{L"Item",L"Node",L"ContentID",L"ClientID"}) if (entry.HasKey(name)) args.Insert(name,entry.GetNamedValue(name));
    if(entry.HasKey(L"Folder"))args.Insert(L"LocalFolder",entry.GetNamedValue(L"Folder"));
    return args;
}
struct Response { IJsonValue result{nullptr}; std::vector<std::byte> data; };
void authenticateServer(HANDLE pipe) {
    ULONG pid{}; check_bool(GetNamedPipeServerProcessId(pipe, &pid));
    handle process(OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid)); check_bool(static_cast<bool>(process));
    HANDLE rawToken{}; check_bool(OpenProcessToken(process.get(), TOKEN_QUERY, &rawToken)); handle token(rawToken);
    auto tokenUser = [](HANDLE value) {
        DWORD size{}; GetTokenInformation(value, TokenUser, nullptr, 0, &size);
        std::vector<std::byte> data(size); check_bool(GetTokenInformation(value, TokenUser, data.data(), size, &size)); return data;
    };
    auto server = tokenUser(token.get()); auto current = tokenUser(GetCurrentProcessToken());
    if (!EqualSid(reinterpret_cast<TOKEN_USER*>(server.data())->User.Sid, reinterpret_cast<TOKEN_USER*>(current.data())->User.Sid)) throw hresult_error(E_ACCESSDENIED, L"The Synctrain pipe belongs to another Windows user.");
}
Response request(std::wstring const& method, JsonObject const& params = JsonObject{}) {
    if (!WaitNamedPipeW(pipePath.c_str(), 5000) && GetLastError() != ERROR_SEM_TIMEOUT) throw_last_error();
    handle pipe(CreateFileW(pipePath.c_str(), GENERIC_READ | GENERIC_WRITE, 0, nullptr, OPEN_EXISTING, 0, nullptr));
    check_bool(pipe.get() != INVALID_HANDLE_VALUE);
    authenticateServer(pipe.get());
    JsonObject message; message.Insert(L"method", text(method)); message.Insert(L"params", params);
    auto line = to_string(message.Stringify()) + "\n"; writeAll(pipe.get(), line.data(), static_cast<DWORD>(line.size()));
    std::string header;std::vector<std::byte> prefix;
    for (;;) {
        std::array<char,65536> buffer{};DWORD read{};check_bool(ReadFile(pipe.get(),buffer.data(),static_cast<DWORD>(buffer.size()),&read,nullptr));if(!read)throw hresult_error(E_FAIL);
        auto end=buffer.data()+read;auto newline=std::find(buffer.data(),end,'\n');header.append(buffer.data(),newline);
        if(header.size()>128*1024*1024)throw hresult_error(E_BOUNDS);
        if(newline!=end){auto data=reinterpret_cast<std::byte const*>(newline+1);prefix.assign(data,reinterpret_cast<std::byte const*>(end));break;}
    }
    auto json = JsonObject::Parse(to_hstring(header));
    if (json.HasKey(L"error")) throw hresult_error(E_FAIL, method+L": "+std::wstring(json.GetNamedString(L"error")));
    Response response;
    if (json.HasKey(L"result")) response.result = json.GetNamedValue(L"result");
    auto length = static_cast<size_t>(json.GetNamedNumber(L"length", 0));
    if (length > 1024 * 1024) throw hresult_error(E_BOUNDS);
    if(prefix.size()>length)throw hresult_error(E_FAIL);response.data.resize(length);std::copy(prefix.begin(),prefix.end(),response.data.begin());size_t offset=prefix.size();
    while (offset < length) { DWORD read{}; check_bool(ReadFile(pipe.get(), response.data.data() + offset, static_cast<DWORD>(length - offset), &read, nullptr)); if (!read) throw hresult_error(E_FAIL); offset += read; }
    return response;
}
void enqueue(std::function<void()> job) {
    { std::lock_guard lock(jobsMutex); if(jobs.size()>=128)throw hresult_error(HRESULT_FROM_WIN32(ERROR_BUSY));jobs.push_back(std::move(job)); } jobsReady.notify_one();
}
void worker() {
    init_apartment(apartment_type::multi_threaded);
    for (;;) {
        std::function<void()> job;
        { std::unique_lock lock(jobsMutex); jobsReady.wait(lock, [] { return stopped || !jobs.empty(); }); if (stopped && jobs.empty()) return; job = std::move(jobs.front()); jobs.pop_front(); }
        try { job(); } catch (hresult_error const& error) { std::wcerr << error.message().c_str() << L'\n'; }
    }
}
CF_OPERATION_INFO operation(CF_CALLBACK_INFO const& info, CF_OPERATION_TYPE type) {
    CF_OPERATION_INFO result{}; result.StructSize = sizeof(result); result.Type = type; result.ConnectionKey = info.ConnectionKey; result.TransferKey = info.TransferKey; result.RequestKey = info.RequestKey; return result;
}
void transfer(CF_CALLBACK_INFO const& info, int64_t offset, int64_t length, void const* data, NTSTATUS status) {
    auto op = operation(info, CF_OPERATION_TYPE_TRANSFER_DATA);
    CF_OPERATION_PARAMETERS params{}; params.ParamSize = sizeof(params); params.TransferData.CompletionStatus = status; params.TransferData.Offset.QuadPart = offset; params.TransferData.Length.QuadPart = length; params.TransferData.Buffer = data;
    check_hresult(CfExecute(&op, &params));
}
JsonObject identity(CF_CALLBACK_INFO const& info) {
    auto bytes = static_cast<char const*>(info.FileIdentity);
    return JsonObject::Parse(to_hstring(std::string(bytes, bytes + info.FileIdentityLength)));
}
void CALLBACK fetchData(CF_CALLBACK_INFO const* info, CF_CALLBACK_PARAMETERS const* params) {
    try {
    auto copy = *info; copy.FileIdentity = nullptr; copy.NormalizedPath = nullptr;
    auto id = identity(*info);
    auto offset = params->FetchData.RequiredFileOffset.QuadPart;
    auto length = std::min(params->FetchData.RequiredLength.QuadPart,info->FileSize.QuadPart-offset);
    auto canceled = std::make_shared<std::atomic<bool>>(false);
    { std::lock_guard lock(jobsMutex); cancellations[info->RequestKey.QuadPart] = canceled; }
    enqueue([copy, id, offset, length, canceled] {
        auto position = offset;
        try {
            while (position < offset + length && !*canceled) {
                auto count = std::min<int64_t>(1024 * 1024, offset + length - position);
                auto args = itemArguments(id); args.Insert(L"Offset", number(position)); args.Insert(L"Length", number(count));
                auto content = request(L"files.read", args);
                if (content.data.empty()) throw hresult_error(E_FAIL);
                if (*canceled) break;
                transfer(copy, position, static_cast<int64_t>(content.data.size()), content.data.data(), 0);
                position += static_cast<int64_t>(content.data.size());
                LARGE_INTEGER total{}, completed{}; total.QuadPart = length; completed.QuadPart = position - offset;
                check_hresult(CfReportProviderProgress(copy.ConnectionKey, copy.TransferKey, total, completed));
            }
        } catch (...) {
            try { throw; } catch (hresult_error const& error) { std::wcerr << L"Fetch: " << error.message().c_str() << L" (" << std::hex << static_cast<uint32_t>(error.code()) << L")\n"; } catch (std::exception const& error) { std::cerr << "Fetch: " << error.what() << "\n"; }
            try { transfer(copy, position, offset + length - position, nullptr, static_cast<NTSTATUS>(0xC000CF12)); } catch (...) {}
        }
        std::lock_guard lock(jobsMutex); cancellations.erase(copy.RequestKey.QuadPart);
    });
    } catch (...) { {std::lock_guard lock(jobsMutex);cancellations.erase(info->RequestKey.QuadPart);} try { transfer(*info,params->FetchData.RequiredFileOffset.QuadPart,params->FetchData.RequiredLength.QuadPart,nullptr,static_cast<NTSTATUS>(0xC000CF12)); } catch (...) {} }
}
void CALLBACK cancelFetch(CF_CALLBACK_INFO const* info, CF_CALLBACK_PARAMETERS const*) {
    std::lock_guard lock(jobsMutex); if (auto found = cancellations.find(info->RequestKey.QuadPart); found != cancellations.end()) *found->second = true;
}
void CALLBACK changed(CF_CALLBACK_INFO const* info, CF_CALLBACK_PARAMETERS const*) {
    if(!info->ProcessInfo || info->ProcessInfo->ProcessId!=GetCurrentProcessId())rescan=true;
}

std::wstring userSID() {
    DWORD size{}; GetTokenInformation(GetCurrentProcessToken(), TokenUser, nullptr, 0, &size);
    std::vector<std::byte> data(size); check_bool(GetTokenInformation(GetCurrentProcessToken(), TokenUser, data.data(), size, &size));
    PWSTR sid{}; check_bool(ConvertSidToStringSidW(reinterpret_cast<TOKEN_USER*>(data.data())->User.Sid, &sid));
    std::wstring result(sid); LocalFree(sid); return result;
}
std::wstring syncRootID() { return L"com.gordonbyrnes.Synctrain!" + userSID() + L"!" + clientID; }
std::wstring rootFileKey() {
    handle file(CreateFileW(windowsPath(rootPath).c_str(),FILE_READ_ATTRIBUTES,FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
    if(file.get()==INVALID_HANDLE_VALUE)throw_last_error();BY_HANDLE_FILE_INFORMATION info{};check_bool(GetFileInformationByHandle(file.get(),&info));
    return std::to_wstring(info.dwVolumeSerialNumber)+L"-"+std::to_wstring((uint64_t(info.nFileIndexHigh)<<32)|info.nFileIndexLow);
}
bool recordedRootOwnership(std::wstring const& key) {
    auto value=request(L"projection.root.get").result;
    if(value && value.ValueType()==JsonValueType::Object){auto proof=value.GetObject();bool matches=proof.GetNamedString(L"ClientID",L"")==clientID && proof.GetNamedString(L"RootKey",L"")==key;if(matches)recoveringRegistration=proof.GetNamedBoolean(L"RecoverRegistration",false);return matches;}
    std::vector<std::byte> storage(8192);auto info=reinterpret_cast<CF_SYNC_ROOT_STANDARD_INFO*>(storage.data());
    if(SUCCEEDED(CfGetSyncRootInfoByPath(rootPath.c_str(),CF_SYNC_ROOT_INFO_STANDARD,storage.data(),static_cast<DWORD>(storage.size()),nullptr)) && info->SyncRootIdentityLength<=4096){
        auto expected=to_string(L"Synctrain v1 "+clientID);
        if(std::string(reinterpret_cast<char*>(info->SyncRootIdentity),info->SyncRootIdentityLength)==expected)return true;
    }
    return legacyOwnsRoot();
}
void saveRootOwnership(bool recovering) {
    JsonObject proof;proof.Insert(L"ClientID",text(clientID));proof.Insert(L"RootKey",text(rootKey));proof.Insert(L"RecoverRegistration",JsonValue::CreateBooleanValue(recovering));JsonObject args;args.Insert(L"Snapshot",proof);request(L"projection.root.save",args);
}
void registerRoot() {
    bool existed=fs::exists(rootPath);
    fs::create_directories(rootPath);
    auto attributes=GetFileAttributesW(windowsPath(rootPath).c_str());
    if (attributes & FILE_ATTRIBUTE_REPARSE_POINT) throw hresult_error(E_INVALIDARG,L"The Cloud Files location must be a physical directory.");
    if (testRegistration) {
        if (!pipePath.starts_with(LR"(\\.\pipe\Synctrain-Test-)")) throw hresult_error(E_ACCESSDENIED);
        CF_SYNC_REGISTRATION registration{}; registration.StructSize=sizeof(registration); registration.ProviderName=L"Synctrain Test"; registration.ProviderVersion=L"1.0";
        CF_SYNC_POLICIES policies{}; policies.StructSize=sizeof(policies); policies.Hydration.Primary=CF_HYDRATION_POLICY_PROGRESSIVE; policies.Population.Primary=CF_POPULATION_POLICY_ALWAYS_FULL;
        policies.InSync=CF_INSYNC_POLICY_TRACK_FILE_LAST_WRITE_TIME | CF_INSYNC_POLICY_TRACK_DIRECTORY_LAST_WRITE_TIME;
        check_hresult(CfRegisterSyncRoot(rootPath.c_str(),&registration,&policies,CF_REGISTER_FLAG_UPDATE)); return;
    }
    bool owned=false;
    for (auto const& registered:StorageProviderSyncRootManager::GetCurrentSyncRoots()) {
        auto existing=fs::path(std::wstring(registered.Path().Path()));
        auto rel=fs::path(rootPath).lexically_relative(existing), reverse=existing.lexically_relative(rootPath);
        auto inside=[](fs::path const& value) { return !value.empty() && *value.begin()!=L".."; };
        if (existing==fs::path(rootPath) && registered.Id()==syncRootID()) owned=true;
        else if(existing==fs::path(rootPath) && std::wstring(registered.Id()).starts_with(L"com.gordonbyrnes.Synctrain!"+userSID()+L"!") && fs::is_empty(rootPath)) {StorageProviderSyncRootManager::Unregister(registered.Id());}
        else if (inside(rel) || inside(reverse)) throw hresult_error(E_INVALIDARG,L"The location overlaps another cloud provider.");
    }
    rootKey=rootFileKey();
    bool registered=owned;
    bool proven=existed && recordedRootOwnership(rootKey);
    if(proven && !registered)recoveringRegistration=true;
    if(existed && !fs::is_empty(rootPath))owned=proven;
    if (existed && !owned && !fs::is_empty(rootPath)) throw hresult_error(E_INVALIDARG,L"Choose an empty location. Existing files have been preserved.");
    wchar_t volume[MAX_PATH]{}, system[MAX_PATH]{};
    check_bool(GetVolumePathNameW(rootPath.c_str(), volume, MAX_PATH)); check_bool(GetVolumeInformationW(volume, nullptr, 0, nullptr, nullptr, nullptr, system, MAX_PATH));
    if (_wcsicmp(system, L"NTFS") || GetDriveTypeW(volume)!=DRIVE_FIXED) throw hresult_error(E_INVALIDARG, L"Cloud Files requires a local NTFS volume.");
    StorageProviderSyncRootInfo root;
    root.Id(syncRootID()); root.Path(StorageFolder::GetFolderFromPathAsync(rootPath).get()); root.DisplayNameResource(L"Synctrain");
    wchar_t module[32768]{};check_bool(GetModuleFileNameW(nullptr,module,32768)!=0);root.IconResource((fs::path(module).parent_path()/L"Synctrain.App.exe").wstring()+L",0"); root.Version(L"1.0.0"); root.ShowSiblingsAsGroup(false);
    root.HydrationPolicy(StorageProviderHydrationPolicy::Progressive);
    root.HydrationPolicyModifier(StorageProviderHydrationPolicyModifier::AutoDehydrationAllowed);
    root.PopulationPolicy(StorageProviderPopulationPolicy::AlwaysFull);
    root.InSyncPolicy(StorageProviderInSyncPolicy::FileLastWriteTime | StorageProviderInSyncPolicy::DirectoryLastWriteTime);
    StorageProviderItemPropertyDefinition state;state.Id(1);state.DisplayNameResource(L"Synctrain status");root.StorageProviderItemPropertyDefinitions().Append(state);
    root.HardlinkPolicy(StorageProviderHardlinkPolicy::None);
    root.Context(CryptographicBuffer::ConvertStringToBinary(L"Synctrain v1 "+clientID, BinaryStringEncoding::Utf8));
    // Persist recovery before registration: a crash after Windows registers the
    // root must not reinterpret package cleanup as user deletions on restart.
    saveRootOwnership(recoveringRegistration);
    StorageProviderSyncRootManager::Register(root);
}
bool validName(std::wstring const& name) {
    if (name.size()>255 || name.empty() || name == L"." || name == L".." || name.back() == L'.' || name.back() == L' ') return false;
    for (auto c : name) if (c < 32 || std::wstring_view(L"<>:\"/\\|?*").find(c) != std::wstring_view::npos) return false;
    auto base = name.substr(0, name.find(L'.')); for (auto& c : base) c = static_cast<wchar_t>(towupper(c));
    if (base == L"CON" || base == L"PRN" || base == L"AUX" || base == L"NUL" || base==L"CONIN$" || base==L"CONOUT$") return false;
    if (base.size() == 4 && (base.starts_with(L"COM") || base.starts_with(L"LPT")) && ((base[3] >= L'1' && base[3] <= L'9') || base[3]==L'¹' || base[3]==L'²' || base[3]==L'³')) return false;
    return true;
}
void projectDirectory(int64_t parent, fs::path const& directory) {
    JsonObject args; args.Insert(L"Parent", number(parent));
    auto entries = request(L"files.children", args).result;
    if (!entries || entries.ValueType() == JsonValueType::Null) return;
    struct CaseLess {bool operator()(std::wstring const& a,std::wstring const& b)const{return CompareStringOrdinal(a.c_str(),static_cast<int>(a.size()),b.c_str(),static_cast<int>(b.size()),TRUE)==CSTR_LESS_THAN;}};
    std::map<std::wstring,int64_t,CaseLess> representatives;
    for (auto const& value:entries.GetArray()) {
        auto entry=value.GetObject();auto name=std::wstring(entry.GetNamedString(L"Name"));
        if(!validName(name))continue;
        auto [selected,first]=representatives.emplace(name,item(entry));
        // Enumeration order must not displace the item already owning this
        // case-insensitive Windows path, including an entire directory tree.
        if(!first && projectedItemAt(directory/name)==item(entry))selected->second=item(entry);
    }
    for (auto const& value : entries.GetArray()) {
        auto entry = value.GetObject(); auto name = std::wstring(entry.GetNamedString(L"Name"));
        retainRemote(entry,false);
        if (!validName(name)) {retainRemote(entry,true);JsonObject issue;issue.Insert(L"Name",text((directory/name).lexically_relative(rootPath).generic_wstring()));issue.Insert(L"Reason",text(L"This name cannot be represented in Windows. Open Files in Synctrain to rename it or save a copy."));if(projectionIssues.Size()<256)projectionIssues.Append(issue);continue;}
        if (representatives.at(name)!=item(entry)) {retainRemote(entry,true);JsonObject issue;issue.Insert(L"Name",text((directory/name).lexically_relative(rootPath).generic_wstring()));issue.Insert(L"Reason",text(L"Another name differs only by letter case. Open Files in Synctrain to rename it or save a copy."));if(projectionIssues.Size()<256)projectionIssues.Append(issue);continue;}
        auto path = directory / name; if (!moveRemote(entry,path)) continue; bool isDirectory = entry.GetNamedNumber(L"Kind") != 0;
        auto id = to_string(entry.Stringify());
        CF_FS_METADATA metadata{}; metadata.FileSize.QuadPart = static_cast<int64_t>(entry.GetNamedNumber(L"Size"));
        metadata.BasicInfo.LastWriteTime.QuadPart=static_cast<int64_t>(entry.GetNamedNumber(L"ModifiedTicks",0));
        metadata.BasicInfo.FileAttributes = isDirectory ? FILE_ATTRIBUTE_DIRECTORY : FILE_ATTRIBUTE_NORMAL;
        if (!isDirectory && !entry.GetNamedBoolean(L"Writable")) metadata.BasicInfo.FileAttributes |= FILE_ATTRIBUTE_READONLY;
        bool recorded=false;
        auto attrs = GetFileAttributesW(windowsPath(path).c_str());
        if (attrs == INVALID_FILE_ATTRIBUTES) {
            CF_PLACEHOLDER_CREATE_INFO create{}; create.RelativeFileName = name.c_str(); create.FsMetadata = metadata; create.FileIdentity = id.data(); create.FileIdentityLength = static_cast<DWORD>(id.size()); create.Flags = CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC | CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION;
            DWORD processed{}; check_hresult(CfCreatePlaceholders(windowsPath(directory).c_str(), &create, 1, CF_CREATE_FLAG_NONE, &processed)); check_hresult(create.Result); recorded=true;
            auto file=attributes(path);auto desired=entry.GetNamedBoolean(L"Keep")?CF_PIN_STATE_PINNED:CF_PIN_STATE_UNPINNED;
            check_hresult(CfSetPinState(file.get(),desired,CF_SET_PIN_FLAG_NONE,nullptr));
            if(desired==CF_PIN_STATE_PINNED && !isDirectory){LARGE_INTEGER offset{},length{};length.QuadPart=CF_EOF;check_hresult(CfHydratePlaceholder(file.get(),offset,length,CF_HYDRATE_FLAG_NONE,nullptr));}
        } else if (attrs & FILE_ATTRIBUTE_REPARSE_POINT) {
            handle file(CreateFileW(windowsPath(path).c_str(), FILE_READ_ATTRIBUTES | FILE_WRITE_ATTRIBUTES | WRITE_DAC, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, nullptr, OPEN_EXISTING, FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS, nullptr));
            if (file.get() == INVALID_HANDLE_VALUE) continue;
            std::vector<std::byte> storage(8192); auto info = reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(storage.data());
            if (SUCCEEDED(CfGetPlaceholderInfo(file.get(), CF_PLACEHOLDER_INFO_STANDARD, info, static_cast<DWORD>(storage.size()), nullptr))) {
                auto old = JsonObject::Parse(to_hstring(std::string(reinterpret_cast<char*>(info->FileIdentity), info->FileIdentityLength)));
                if(item(old)!=item(entry) || old.GetNamedString(L"ClientID",L"")!=clientID) {
                    retainRemote(entry,true);JsonObject issue;issue.Insert(L"Name",text((directory/name).lexically_relative(rootPath).generic_wstring()));issue.Insert(L"Reason",text(L"Another item occupies this Windows path. Its content has been preserved; finish pending changes or rename the conflicting item in Synctrain."));if(projectionIssues.Size()<256)projectionIssues.Append(issue);continue;
                }
                bool contentChanged=old.GetNamedNumber(L"Node")!=entry.GetNamedNumber(L"Node");
                bool metadataChanged=old.GetNamedString(L"ModTime",L"")!=entry.GetNamedString(L"ModTime",L"") || old.GetNamedBoolean(L"Writable",false)!=entry.GetNamedBoolean(L"Writable",false) || old.GetNamedString(L"ContentID",L"")!=entry.GetNamedString(L"ContentID",L"");
                if ((contentChanged || metadataChanged) && info->InSyncState == CF_IN_SYNC_STATE_IN_SYNC) {
                    ProtectedFile exclusive(path);
                    if(!samePlaceholder(CfGetWin32HandleFromProtectedHandle(exclusive.value),old)){rescan=true;continue;}
                    if (contentChanged && !isDirectory && info->PinState==CF_PIN_STATE_PINNED) {check_hresult(CfSetPinState(exclusive.value,CF_PIN_STATE_UNSPECIFIED,CF_SET_PIN_FLAG_NONE,nullptr));info->PinState=CF_PIN_STATE_UNSPECIFIED;}
                    check_hresult(CfUpdatePlaceholder(exclusive.value, &metadata, id.data(), static_cast<DWORD>(id.size()), nullptr, 0, (!isDirectory && contentChanged ? CF_UPDATE_FLAG_DEHYDRATE : CF_UPDATE_FLAG_NONE) | CF_UPDATE_FLAG_MARK_IN_SYNC | CF_UPDATE_FLAG_VERIFY_IN_SYNC, nullptr, nullptr)); recorded=true;
                }
                auto desired=entry.GetNamedBoolean(L"Keep") ? CF_PIN_STATE_PINNED : CF_PIN_STATE_UNPINNED;
                // A local capture's USN remains its content proof until the
                // shared queue commits it. Defer our own pin metadata changes.
                if (info->InSyncState==CF_IN_SYNC_STATE_IN_SYNC && info->PinState!=desired) {check_hresult(CfSetPinState(file.get(),desired,CF_SET_PIN_FLAG_NONE,nullptr));recorded=true;}
                if (info->InSyncState==CF_IN_SYNC_STATE_IN_SYNC && desired == CF_PIN_STATE_PINNED) {
                    LARGE_INTEGER offset{}, length{}; length.QuadPart = CF_EOF;
                    if (!isDirectory) check_hresult(CfHydratePlaceholder(file.get(), offset, length, CF_HYDRATE_FLAG_NONE, nullptr));
                }
            }
        }
        if (recorded) projected(path,entry);
        if (isDirectory) projectDirectory(static_cast<int64_t>(entry.GetNamedNumber(L"Item")), path);
    }
}
#include "reconcile.h"
#include "cache.h"
#include "shell.h"
#include "watcher.h"

BOOL WINAPI stop(DWORD) { stopped = true; jobsReady.notify_all(); return TRUE; }
}

#include "recycle.h"
int wmain(int argc, wchar_t** argv) {
    try {
        if(argc==3 && std::wstring_view(argv[1])==L"--recycle"){init_apartment(apartment_type::single_threaded);return recycleItem(argv[2]);}
        init_apartment(apartment_type::multi_threaded);
        std::wstring ready,probe; bool shellOnly=false,legacyProbe=false;
        for (int i = 1; i + 1 < argc; i += 2) { std::wstring arg(argv[i]); if (arg == L"--root") rootPath = argv[i + 1]; else if (arg == L"--pipe") pipePath = argv[i + 1]; else if (arg == L"--ready") ready = argv[i + 1]; else if (arg == L"--client") clientID = argv[i+1]; else if(arg==L"--recover-registration") recoveringRegistration=std::wstring(argv[i+1])==L"true"; else if (arg==L"--test") testRegistration=std::wstring(argv[i+1])==L"true"; else if (arg==L"--shutdown") shutdownPath=argv[i+1]; else if (arg==L"--quiesce") quiescePath=argv[i+1]; else if (arg==L"--detach") detachPath=argv[i+1]; else if (arg==L"--approval") approvalPath=argv[i+1]; else if(arg==L"--probe-legacy-root") legacyProbe=std::wstring(argv[i+1])==L"true"; else if(arg==L"--probe-thumbnail") probe=argv[i+1]; else if(arg==L"--shell") shellOnly=std::wstring(argv[i+1])==L"true"; }
        if(!probe.empty())return probeThumbnail(probe);
        if(shellOnly)return serveShell();
        if (rootPath.empty() || pipePath.empty() || clientID.empty()) return 2;
        if(legacyProbe){if(!testRegistration || !pipePath.starts_with(LR"(\\.\pipe\Synctrain-Test-)"))throw hresult_error(E_ACCESSDENIED);std::cout<<(legacyOwnsRoot()?"owned":"unowned");return 0;}
        registerRoot();
        std::unique_ptr<ShellRegistration> shell; if(!testRegistration)shell=std::make_unique<ShellRegistration>();
        CF_CALLBACK_REGISTRATION callbacks[] = {{CF_CALLBACK_TYPE_FETCH_DATA, fetchData}, {CF_CALLBACK_TYPE_CANCEL_FETCH_DATA, cancelFetch}, {CF_CALLBACK_TYPE_NOTIFY_FILE_CLOSE_COMPLETION, changed}, {CF_CALLBACK_TYPE_NOTIFY_RENAME_COMPLETION, changed}, {CF_CALLBACK_TYPE_NOTIFY_DELETE_COMPLETION, changed}, CF_CALLBACK_REGISTRATION_END};
        check_hresult(CfConnectSyncRoot(rootPath.c_str(), callbacks, nullptr, CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO | CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH | CF_CONNECT_FLAG_BLOCK_SELF_IMPLICIT_HYDRATION, &connection));
        DirectoryWatcher watcher(rootPath);
        if (!ready.empty()) { std::ofstream marker(fs::path(ready), std::ios::trunc); marker << "ready\n"; }
        SetConsoleCtrlHandler(stop, TRUE);
        std::vector<std::thread> workers; for (int i = 0; i < 4; ++i) workers.emplace_back(worker);
        bool detached=false;
        while (!stopped) {
            if (!detachPath.empty() && fs::exists(detachPath)) {
                try { detachLocation();detached=true;break; }
                catch (hresult_error const& error) { std::ofstream marker(fs::path(detachPath+L".error"));marker<<to_string(error.message()); }
                catch (std::exception const& error) { std::ofstream marker(fs::path(detachPath+L".error"));marker<<error.what(); }
                Sleep(100);continue;
            }
            if (!shutdownPath.empty() && fs::exists(shutdownPath)) break;
            try {
                std::lock_guard lock(changesMutex);
                bool quiescing=!quiescePath.empty() && fs::exists(quiescePath);
                if (!quiescing || !fs::exists(quiescePath+L".ready")) {
                    reconcile();
                    if(!quiescing)trimNativeCache();
                    if (quiescing) { std::ofstream marker(fs::path(quiescePath+L".ready")); marker<<"ready\n"; }
                }
            }
            catch (hresult_error const& error) { rescan=true; if (!quiescePath.empty() && fs::exists(quiescePath)) { std::ofstream marker(fs::path(quiescePath+L".error")); marker<<to_string(error.message()); } std::wcerr << L"Projection: " << error.message().c_str() << L'\n'; }
            catch (std::exception const& error) { rescan=true; std::cerr << "Projection: " << error.what() << '\n'; }
            Sleep(1000);
        }
        auto disconnected=CfDisconnectSyncRoot(connection);
        if(disconnected!=HRESULT_FROM_WIN32(ERROR_NOT_FOUND))check_hresult(disconnected);
        stopped=true; jobsReady.notify_all(); for (auto& thread : workers) thread.join();
        if (testRegistration) check_hresult(CfUnregisterSyncRoot(rootPath.c_str()));
        else if (detached) {
            try {StorageProviderSyncRootManager::Unregister(syncRootID());}
            catch(hresult_error const& error){
                if(error.code()!=HRESULT_FROM_WIN32(ERROR_NOT_FOUND))throw;
                // Cleanup is idempotent when Windows already removed the root.
                for(auto const& root:StorageProviderSyncRootManager::GetCurrentSyncRoots())if(root.Id()==syncRootID())throw;
            }
        }
        return 0;
    } catch (hresult_error const& error) { std::wcerr << error.message().c_str() << L" (" << std::hex << static_cast<uint32_t>(error.code()) << L")\n"; return 1; }
    catch (std::exception const& error) { std::cerr << error.what() << '\n'; return 1; }
}
