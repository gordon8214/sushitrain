// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Out-of-process shell handlers. They request bounded previews and status from
// the host; none opens the cloud file's data stream to generate a thumbnail.
std::atomic<int> shellObjects{},shellLocks{};
std::atomic<ULONGLONG> shellLastUse{GetTickCount64()};
struct ShellLifetime {
    ShellLifetime(){++shellObjects;shellLastUse=GetTickCount64();}
    ~ShellLifetime(){--shellObjects;shellLastUse=GetTickCount64();}
};
JsonObject shellIdentity(IShellItem* item) {
    if(!item)throw hresult_error(E_INVALIDARG);
    PWSTR raw{};check_hresult(item->GetDisplayName(SIGDN_FILESYSPATH,&raw));std::wstring path(raw);CoTaskMemFree(raw);
    handle file(CreateFileW(windowsPath(path).c_str(),FILE_READ_ATTRIBUTES,FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE,nullptr,OPEN_EXISTING,FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS,nullptr));
    if(file.get()==INVALID_HANDLE_VALUE) throw_last_error();
    std::vector<std::byte> buffer(8192);auto info=reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(buffer.data());
    check_hresult(CfGetPlaceholderInfo(file.get(),CF_PLACEHOLDER_INFO_STANDARD,info,static_cast<DWORD>(buffer.size()),nullptr));
    auto identity=JsonObject::Parse(to_hstring(std::string(reinterpret_cast<char*>(info->FileIdentity),info->FileIdentityLength)));identity.Insert(L"NativeInSync",JsonValue::CreateBooleanValue(info->InSyncState==CF_IN_SYNC_STATE_IN_SYNC));return identity;
}
struct __declspec(uuid("36F7C1C1-6702-4E7E-9FF8-0497045529CE")) CloudThumbnail : implements<CloudThumbnail,IInitializeWithItem,IThumbnailProvider>,ShellLifetime {
    com_ptr<IShellItem> selected;
    HRESULT __stdcall Initialize(IShellItem* item,DWORD) noexcept override {
        if(!item)return E_INVALIDARG;selected.copy_from(item);return S_OK;
    }
    HRESULT __stdcall GetThumbnail(UINT size,HBITMAP* bitmap,WTS_ALPHATYPE* alpha) noexcept override {
        if(!bitmap || !alpha)return E_POINTER;*bitmap=nullptr;*alpha=WTSAT_UNKNOWN;
        try {
            auto args=itemArguments(shellIdentity(selected.get()));args.Insert(L"Length",number(std::clamp<UINT>(size,16,1024)));
            auto response=request(L"files.thumbnail",args);
            com_ptr<IStream> stream;stream.attach(SHCreateMemStream(reinterpret_cast<BYTE const*>(response.data.data()),static_cast<UINT>(response.data.size())));check_bool(static_cast<bool>(stream));
            auto factory=create_instance<IWICImagingFactory>(CLSID_WICImagingFactory);
            com_ptr<IWICBitmapDecoder> decoder;check_hresult(factory->CreateDecoderFromStream(stream.get(),nullptr,WICDecodeMetadataCacheOnDemand,decoder.put()));
            com_ptr<IWICBitmapFrameDecode> frame;check_hresult(decoder->GetFrame(0,frame.put()));
            UINT width{},height{};check_hresult(frame->GetSize(&width,&height));if(!width || !height || width>2048 || height>2048)throw hresult_error(E_BOUNDS);
            auto side=std::clamp<UINT>(size,16,1024);auto scale=std::min(1.0,std::min(double(side)/width,double(side)/height));
            auto w=std::max(1U,static_cast<UINT>(width*scale)),h=std::max(1U,static_cast<UINT>(height*scale));
            com_ptr<IWICBitmapScaler> scaler;check_hresult(factory->CreateBitmapScaler(scaler.put()));check_hresult(scaler->Initialize(frame.get(),w,h,WICBitmapInterpolationModeFant));
            com_ptr<IWICFormatConverter> converter;check_hresult(factory->CreateFormatConverter(converter.put()));check_hresult(converter->Initialize(scaler.get(),GUID_WICPixelFormat32bppPBGRA,WICBitmapDitherTypeNone,nullptr,0,WICBitmapPaletteTypeCustom));
            BITMAPINFO info{};info.bmiHeader.biSize=sizeof(BITMAPINFOHEADER);info.bmiHeader.biWidth=static_cast<LONG>(w);info.bmiHeader.biHeight=-static_cast<LONG>(h);info.bmiHeader.biPlanes=1;info.bmiHeader.biBitCount=32;info.bmiHeader.biCompression=BI_RGB;
            void* pixels{};auto result=CreateDIBSection(nullptr,&info,DIB_RGB_COLORS,&pixels,nullptr,0);if(!result)throw_last_error();
            auto hr=converter->CopyPixels(nullptr,w*4,w*h*4,static_cast<BYTE*>(pixels));if(FAILED(hr)){DeleteObject(result);check_hresult(hr);}
            *bitmap=result;*alpha=WTSAT_ARGB;return S_OK;
        }catch(...){return to_hresult();}
    }
};
struct __declspec(uuid("36F7C1C1-6702-4E7E-9FF8-0497045529CF")) PreviousVersions : implements<PreviousVersions,IExplorerCommand>,ShellLifetime {
    HRESULT __stdcall GetTitle(IShellItemArray*,PWSTR* text) noexcept override{return SHStrDupW(L"Previous versions in Synctrain",text);}
    HRESULT __stdcall GetIcon(IShellItemArray*,PWSTR* icon) noexcept override{if(!icon)return E_POINTER;*icon=nullptr;return E_NOTIMPL;}
    HRESULT __stdcall GetToolTip(IShellItemArray*,PWSTR* text) noexcept override{if(!text)return E_POINTER;*text=nullptr;return E_NOTIMPL;}
    HRESULT __stdcall GetCanonicalName(GUID* id) noexcept override{if(!id)return E_POINTER;*id=__uuidof(PreviousVersions);return S_OK;}
    HRESULT __stdcall GetState(IShellItemArray* items,BOOL,EXPCMDSTATE* state) noexcept override{if(!state)return E_POINTER;DWORD count{};*state=items && SUCCEEDED(items->GetCount(&count)) && count==1 ? ECS_ENABLED:ECS_HIDDEN;return S_OK;}
    HRESULT __stdcall GetFlags(EXPCMDFLAGS* flags) noexcept override{if(!flags)return E_POINTER;*flags=ECF_DEFAULT;return S_OK;}
    HRESULT __stdcall EnumSubCommands(IEnumExplorerCommand** commands) noexcept override{if(!commands)return E_POINTER;*commands=nullptr;return E_NOTIMPL;}
    HRESULT __stdcall Invoke(IShellItemArray* items,IBindCtx*) noexcept override {
        try {
            if(!items)return E_INVALIDARG;com_ptr<IShellItem> selected;check_hresult(items->GetItemAt(0,selected.put()));auto entry=shellIdentity(selected.get());
            auto uri=L"synctrain://versions?item="+std::to_wstring(static_cast<int64_t>(entry.GetNamedNumber(L"Item")))+L"&client="+std::wstring(entry.GetNamedString(L"ClientID"));
            auto result=ShellExecuteW(nullptr,L"open",uri.c_str(),nullptr,nullptr,SW_SHOWNORMAL);
            return reinterpret_cast<INT_PTR>(result)>32 ? S_OK:E_FAIL;
        }catch(...){return to_hresult();}
    }
};
struct __declspec(uuid("36F7C1C1-6702-4E7E-9FF8-0497045529D0")) CloudState : implements<CloudState,IStorageProviderItemPropertySource>,ShellLifetime {
    Windows::Foundation::Collections::IIterable<StorageProviderItemProperty> GetItemProperties(hstring const& path) {
        std::vector<StorageProviderItemProperty> properties;
        try {
            com_ptr<IShellItem> item;check_hresult(SHCreateItemFromParsingName(path.c_str(),nullptr,IID_PPV_ARGS(item.put())));
            auto identity=shellIdentity(item.get());auto state=std::wstring(request(L"files.state",itemArguments(identity)).result.GetString());
            std::wstring label=state==L"pending"?L"Uploading changes":state==L"local-only"?L"Local only":state==L"failed" || state==L"blocked"?L"Needs attention":state==L"remote"?L"Available online":state==L"partial"?L"Downloading":state==L"kept"?L"Kept on this device":L"Synchronized";
            if(!identity.GetNamedBoolean(L"NativeInSync",true) && (state==L"remote" || state==L"partial" || state==L"local" || state==L"kept"))label=L"Local changes pending";
            StorageProviderItemProperty property;property.Id(1);property.Value(label);properties.push_back(property);
        }catch(...){StorageProviderItemProperty property;property.Id(1);property.Value(L"Synctrain is unavailable");properties.push_back(property);}
        return single_threaded_vector(std::move(properties));
    }
};
template<class T> struct ShellFactory : implements<ShellFactory<T>,IClassFactory> {
    HRESULT __stdcall CreateInstance(IUnknown* outer,REFIID id,void** object) noexcept override {
        if(!object)return E_POINTER;*object=nullptr;if(outer)return CLASS_E_NOAGGREGATION;
        try {auto instance=make_self<T>();return instance->QueryInterface(id,object);}catch(...){return to_hresult();}
    }
    HRESULT __stdcall LockServer(BOOL lock) noexcept override{if(lock)++shellLocks;else --shellLocks;shellLastUse=GetTickCount64();return S_OK;}
};
struct ShellRegistration {
    std::vector<DWORD> cookies;
    template<class T> void add(){auto factory=make_self<ShellFactory<T>>();DWORD cookie{};check_hresult(CoRegisterClassObject(__uuidof(T),factory.get(),CLSCTX_LOCAL_SERVER,REGCLS_MULTIPLEUSE,&cookie));cookies.push_back(cookie);}
    ShellRegistration(){add<CloudThumbnail>();add<PreviousVersions>();add<CloudState>();}
    ~ShellRegistration(){for(auto cookie:cookies)CoRevokeClassObject(cookie);}
};
int serveShell() {
    pipePath=LR"(\\.\pipe\Synctrain-)"+userSID();
    // Shell activation can start the signed-in user's background agent after an
    // explicit file action. The supervisor's lock handles concurrent activations.
    if(!WaitNamedPipeW(pipePath.c_str(),100)) {
        wchar_t executable[32768]{};check_bool(GetModuleFileNameW(nullptr,executable,32768)!=0);
        auto host=(fs::path(executable).parent_path()/L"synctrain-host.exe").wstring();auto command=L"\""+host+L"\"";
        STARTUPINFOW startup{};startup.cb=sizeof(startup);PROCESS_INFORMATION process{};
        check_bool(CreateProcessW(host.c_str(),command.data(),nullptr,nullptr,FALSE,CREATE_NO_WINDOW,nullptr,nullptr,&startup,&process));CloseHandle(process.hThread);CloseHandle(process.hProcess);
    }
    ShellRegistration registration;
    for(;;){Sleep(500);if(shellObjects==0 && shellLocks==0 && GetTickCount64()-shellLastUse>10000){check_hresult(CoSuspendClassObjects());if(shellObjects==0 && shellLocks==0)break;check_hresult(CoResumeClassObjects());}}
    return 0;
}

int probeThumbnail(fs::path const& path) {
    auto diskBytes=[&] {auto file=attributes(path);std::vector<std::byte> buffer(8192);auto info=reinterpret_cast<CF_PLACEHOLDER_STANDARD_INFO*>(buffer.data());check_hresult(CfGetPlaceholderInfo(file.get(),CF_PLACEHOLDER_INFO_STANDARD,info,static_cast<DWORD>(buffer.size()),nullptr));return info->OnDiskDataSize.QuadPart;};
    auto before=diskBytes();com_ptr<IShellItem> item;check_hresult(SHCreateItemFromParsingName(path.c_str(),nullptr,IID_PPV_ARGS(item.put())));
    auto handler=make_self<CloudThumbnail>();check_hresult(handler->Initialize(item.get(),STGM_READ));HBITMAP bitmap{};WTS_ALPHATYPE alpha{};check_hresult(handler->GetThumbnail(256,&bitmap,&alpha));
    BITMAP info{};GetObjectW(bitmap,sizeof(info),&info);DeleteObject(bitmap);auto after=diskBytes();
    if(info.bmWidth<=0 || info.bmWidth>256 || before!=after)throw hresult_error(E_FAIL,L"Thumbnail extraction materialized the file or returned an invalid bitmap.");
    std::wcout<<L"thumbnailWidth="<<info.bmWidth<<L" onDiskBefore="<<before<<L" onDiskAfter="<<after<<L"\n";return 0;
}
