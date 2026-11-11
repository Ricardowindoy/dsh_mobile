import java.util.Properties
import java.io.InputStreamReader

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("rust")
}

val tauriProperties = Properties().apply {
    val propFile = file("tauri.properties")
    if (propFile.exists()) {
        propFile.inputStream().use { load(it) }
    }
}

// 加载签名 keystore 配置（位于项目根 mobile/android-keystore 目录）。
// 必须显式用 UTF-8 reader 读取，避免非 ASCII 路径被 ISO-8859-1 误解。
// app 模块的 rootProject 是 gen/android，需要向上 3 级回到 mobile/。
val keystoreProperties = Properties().apply {
    val propFile = rootProject.file("../../../android-keystore/keystore.properties")
    if (propFile.exists()) {
        InputStreamReader(propFile.inputStream(), Charsets.UTF_8).use { load(it) }
    }
}

android {
    compileSdk = 36
    namespace = "com.namelessric.dsh_mobile"
    defaultConfig {
        // Relay 地址由用户填写，局域网部署通常使用 HTTP。若 release 禁止
        // cleartext，地址可达时 WebView 仍会直接报 Network Security 错误。
        manifestPlaceholders["usesCleartextTraffic"] = "true"
        applicationId = "com.namelessric.dsh_mobile"
        minSdk = 24
        targetSdk = 36
        versionCode = tauriProperties.getProperty("tauri.android.versionCode", "1").toInt()
        versionName = tauriProperties.getProperty("tauri.android.versionName", "1.0")
    }
    signingConfigs {
        create("release") {
            keystoreProperties["storeFile"]?.let { storeFile = file(it as String) }
            storePassword = keystoreProperties["storePassword"] as String?
            keyAlias = keystoreProperties["keyAlias"] as String?
            keyPassword = keystoreProperties["keyPassword"] as String?
            // 启用 v1/v2/v3 签名以兼容现代 Android
            enableV1Signing = true
            enableV2Signing = true
            enableV3Signing = true
        }
    }
    buildTypes {
        getByName("debug") {
            manifestPlaceholders["usesCleartextTraffic"] = "true"
            isDebuggable = true
            isJniDebuggable = true
            isMinifyEnabled = false
            packaging {                jniLibs.keepDebugSymbols.add("*/arm64-v8a/*.so")
                jniLibs.keepDebugSymbols.add("*/armeabi-v7a/*.so")
                jniLibs.keepDebugSymbols.add("*/x86/*.so")
                jniLibs.keepDebugSymbols.add("*/x86_64/*.so")
            }
        }
        getByName("release") {
            isMinifyEnabled = true
            isShrinkResources = true
            signingConfig = signingConfigs.getByName("release")
            proguardFiles(
                *fileTree(".") { include("**/*.pro") }
                    .plus(getDefaultProguardFile("proguard-android-optimize.txt"))
                    .toList().toTypedArray()
            )
        }
    }
    kotlinOptions {
        jvmTarget = "1.8"
    }
    buildFeatures {
        buildConfig = true
    }
}

rust {
    rootDirRel = "../../../"
}

dependencies {
    implementation("androidx.webkit:webkit:1.14.0")
    implementation("androidx.appcompat:appcompat:1.7.1")
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-ktx:1.10.1")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.lifecycle:lifecycle-process:2.10.0")
    // 相机扫码已改走 WebView getUserMedia + jsQR（见 main.js），不再使用
    // barcode-scanner 插件的 MLKit/CameraX scan()：该 native 路径在
    // vivo/Android 16 上扫到码后会整进程闪退（1.0.4 日志实证），且
    // bundled 解码器会让 APK 多背 5.8MB。插件仅保留用于声明 CAMERA 权限。
    testImplementation("junit:junit:4.13.2")
    androidTestImplementation("androidx.test.ext:junit:1.1.4")
    androidTestImplementation("androidx.test.espresso:espresso-core:3.5.0")
}

apply(from = "tauri.build.gradle.kts")